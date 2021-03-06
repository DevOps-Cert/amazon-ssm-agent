// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package configurepackage implements the ConfigurePackage plugin.
package configurepackage

import (
	"errors"
	"fmt"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/iohandler"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/birdwatcher"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/installer"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/localpackages"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/packageservice"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/ssms3"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/trace"
	"github.com/aws/amazon-ssm-agent/agent/task"
)

const (
	// InstallAction represents the json command to install package
	InstallAction = "Install"
	// UninstallAction represents the json command to uninstall package
	UninstallAction = "Uninstall"
)

// Plugin is the type for the configurepackage plugin.
type Plugin struct {
	packageServiceSelector func(tracer trace.Tracer, serviceEndpoint string, localrepo localpackages.Repository) packageservice.PackageService
	localRepository        localpackages.Repository
}

// ConfigurePackagePluginInput represents one set of commands executed by the ConfigurePackage plugin.
type ConfigurePackagePluginInput struct {
	contracts.PluginInput
	Name       string `json:"name"`
	Version    string `json:"version"`
	Action     string `json:"action"`
	Source     string `json:"source"`
	Repository string `json:"repository"`
}

// NewPlugin returns a new instance of the plugin.
func NewPlugin() (*Plugin, error) {
	var plugin Plugin

	plugin.localRepository = localpackages.NewRepository()
	plugin.packageServiceSelector = selectService

	return &plugin, nil
}

// prepareConfigurePackage ensures the packages are present with the right version for the scenario requested and returns their installers
func prepareConfigurePackage(
	tracer trace.Tracer,
	config contracts.Configuration,
	repository localpackages.Repository,
	packageService packageservice.PackageService,
	input *ConfigurePackagePluginInput,
	packageArn string,
	version string,
	isSameAsCache bool,
	output contracts.PluginOutputter) (inst installer.Installer, uninst installer.Installer, installState localpackages.InstallState, installedVersion string) {

	prepareTrace := tracer.BeginSection(fmt.Sprintf("prepare %s", input.Action))
	defer prepareTrace.End()

	switch input.Action {
	case InstallAction:
		// get version information
		trace := tracer.BeginSection("determine version to install")
		installedVersion, installState = getVersionToInstall(tracer, repository, packageArn)
		trace.AppendDebugf("installed: %v in state %v, to install: %v", installedVersion, installState, version).End()

		// ensure manifest file and package
		var err error
		trace = tracer.BeginSection("ensure package is locally available")
		inst, err = ensurePackage(tracer, repository, packageService, packageArn, version, isSameAsCache, config)
		if err != nil {
			trace.WithError(err).End()
			output.MarkAsFailed(nil, nil)
			return
		}
		trace.End()

		// if different version is installed, uninstall
		// * Check if the version is already installed using the packageArn
		// * If the version exists, but the local manifest is different, reinstall the package
		// * Return success if the package is already installed
		trace = tracer.BeginSection("ensure old package is locally available")
		if !(installedVersion == "" || installState == localpackages.None) && (installedVersion != version || !isSameAsCache) {
			uninst, err = ensurePackage(tracer, repository, packageService, packageArn, installedVersion, isSameAsCache, config)
			if err != nil {
				trace.WithError(err)
			}
		}
		trace.End()

	case UninstallAction:
		// get version information
		var err error
		trace := tracer.BeginSection("determine version to uninstall")
		installedVersion, installState = getVersionToUninstall(tracer, repository, packageArn)

		// if the input.Version is not specified, or is "latest", uninstall the installedVersion
		if input.Version == "" || packageservice.IsLatest(input.Version) {
			version = installedVersion
		}

		//return success if the version is already uninstalled
		//return success if the installState is None or Uninstalled
		if (installedVersion == "" || version != installedVersion) ||
			(installState == localpackages.None || installState == localpackages.Uninstalled) {
			trace.AppendDebugf("version: %v is not installed", version).End()
			installState = localpackages.None
			output.MarkAsSucceeded()
			return
		}

		trace.AppendDebugf("installed: %v in state: %v", installedVersion, installState).End()

		// ensure manifest file and package
		trace = tracer.BeginSection("ensure package is locally available")
		uninst, err = ensurePackage(tracer, repository, packageService, packageArn, installedVersion, isSameAsCache, config)
		if err != nil {
			trace.WithError(err).End()
			output.MarkAsFailed(nil, nil)
			return
		}
		trace.End()

	default:
		prepareTrace.AppendErrorf("unsupported action: %v", input.Action)
		output.MarkAsFailed(nil, nil)
		return
	}

	return inst, uninst, installState, installedVersion
}

// ensurePackage validates local copy of the manifest and package and downloads if needed, returning the installer
func ensurePackage(
	tracer trace.Tracer,
	repository localpackages.Repository,
	packageService packageservice.PackageService,
	packageName string,
	version string,
	isSameAsCache bool,
	config contracts.Configuration) (installer.Installer, error) {

	pkgTrace := tracer.BeginSection("ensure package is available locally")

	currentState, currentVersion := repository.GetInstallState(tracer, packageName)
	if err := repository.ValidatePackage(tracer, packageName, version); err != nil ||
		(currentVersion == version && (currentState == localpackages.Failed || !isSameAsCache)) {
		pkgTrace.AppendDebugf("Current %v Target %v State %v", currentVersion, version, currentState).End()
		pkgTrace.AppendDebugf("Refreshing package content for %v %v", packageName, version).End()
		if err = repository.RefreshPackage(tracer, packageName, version, packageService.PackageServiceName(), buildDownloadDelegate(tracer, packageService, packageName, version)); err != nil {
			pkgTrace.WithError(err).End()
			return nil, err
		}
		if err = repository.ValidatePackage(tracer, packageName, version); err != nil {
			// TODO: Remove from repository?
			pkgTrace.WithError(err).End()
			return nil, err
		}
	}

	pkgTrace.End()
	return repository.GetInstaller(tracer, config, packageName, version), nil
}

// buildDownloadDelegate constructs the delegate used by the repository to download a package from the service
func buildDownloadDelegate(tracer trace.Tracer, packageService packageservice.PackageService, packageName string, version string) func(trace.Tracer, string) error {
	return func(tracer trace.Tracer, targetDirectory string) error {
		trace := tracer.BeginSection("download artifact")
		filePath, err := packageService.DownloadArtifact(tracer, packageName, version)
		if err != nil {
			trace.WithError(err).End()
			return err
		}

		// TODO: Consider putting uncompress into the ssminstaller new and not deleting it (since the zip is the repository-validatable artifact)
		if uncompressErr := filesysdep.Uncompress(filePath, targetDirectory); uncompressErr != nil {
			trace.WithError(uncompressErr).End()
			return fmt.Errorf("failed to extract package installer package %v from %v, %v", filePath, targetDirectory, uncompressErr.Error())
		}

		// NOTE: this could be considered a warning - it likely points to a real problem, but if uncompress succeeded, we could continue
		// delete compressed package after using
		if cleanupErr := filesysdep.RemoveAll(filePath); cleanupErr != nil {
			trace.WithError(cleanupErr).End()
			return fmt.Errorf("failed to delete compressed package %v, %v", filePath, cleanupErr.Error())
		}

		trace.End()
		return nil
	}
}

// getVersionToInstall decides which version to install and whether there is an existing version (that is not in the process of installing)
func getVersionToInstall(
	tracer trace.Tracer,
	repository localpackages.Repository,
	packageArn string) (installedVersion string, installState localpackages.InstallState) {

	installedVersion = repository.GetInstalledVersion(tracer, packageArn)
	currentState, currentVersion := repository.GetInstallState(tracer, packageArn)
	if currentState == localpackages.Failed {
		// This will only happen if install failed with no previous successful install or if rollback failed
		installedVersion = currentVersion
	}

	return installedVersion, currentState
}

// getVersionToUninstall decides which version to uninstall
func getVersionToUninstall(
	tracer trace.Tracer,
	repository localpackages.Repository,
	packageArn string) (string, localpackages.InstallState) {

	installedVersion := repository.GetInstalledVersion(tracer, packageArn)
	currentState, _ := repository.GetInstallState(tracer, packageArn)

	if installedVersion == "" {
		return "", localpackages.None
	}

	return installedVersion, currentState
}

// parseAndValidateInput marshals raw JSON and returns the result of input validation or an error
func parseAndValidateInput(rawPluginInput interface{}) (*ConfigurePackagePluginInput, error) {
	var input ConfigurePackagePluginInput
	var err error
	if err = jsonutil.Remarshal(rawPluginInput, &input); err != nil {
		return nil, fmt.Errorf("invalid format in plugin properties %v; \nerror %v", rawPluginInput, err)
	}

	if valid, err := validateInput(&input); !valid {
		return nil, fmt.Errorf("invalid input: %v", err)
	}

	return &input, nil
}

// validateInput ensures the plugin input matches the defined schema
func validateInput(input *ConfigurePackagePluginInput) (valid bool, err error) {
	// source not yet supported
	if input.Source != "" {
		return false, errors.New("source parameter is not supported in this version")
	}

	// ensure non-empty name
	if input.Name == "" {
		return false, errors.New("empty name field")
	}

	// dump any unsupported value for Repository
	if input.Repository != "beta" && input.Repository != "gamma" {
		input.Repository = ""
	}

	return true, nil
}

// checkAlreadyInstalled returns true if the version being installed is already in a valid installed state
func checkAlreadyInstalled(
	tracer trace.Tracer,
	context context.T,
	repository localpackages.Repository,
	installedVersion string,
	installState localpackages.InstallState,
	inst installer.Installer,
	uninst installer.Installer,
	output contracts.PluginOutputter) bool {

	checkTrace := tracer.BeginSection("check if already installed")
	defer checkTrace.End()

	if inst != nil {
		targetVersion := inst.Version()
		packageName := inst.PackageName()
		var instToCheck installer.Installer

		// TODO: When existing packages have idempotent installers and no reboot loops, remove this check for installing packages and allow the install to continue until it reports success without reboot
		if uninst != nil && installState == localpackages.RollbackInstall {
			// This supports rollback to a version whose installer contains an unconditional reboot
			instToCheck = uninst
		}
		if (targetVersion == installedVersion &&
			(installState == localpackages.Installed || installState == localpackages.Unknown)) ||
			installState == localpackages.Installing {
			instToCheck = inst
		}
		if instToCheck != nil {
			validateTrace := tracer.BeginSection(fmt.Sprintf("run validate for %s/%s", instToCheck.PackageName(), instToCheck.Version()))

			validateOutput := instToCheck.Validate(tracer, context)
			validateTrace.WithExitcode(int64(validateOutput.GetExitCode()))

			if validateOutput.GetStatus() == contracts.ResultStatusSuccess {
				if installState == localpackages.Installing {
					validateTrace.AppendInfof("Successfully installed %v %v", packageName, targetVersion)
					if uninst != nil {
						cleanupAfterUninstall(tracer, repository, uninst, output)
					}
					output.MarkAsSucceeded()
				} else if installState == localpackages.RollbackInstall {
					validateTrace.AppendInfof("Failed to install %v %v, successfully rolled back to %v %v", uninst.PackageName(), uninst.Version(), inst.PackageName(), inst.Version())
					cleanupAfterUninstall(tracer, repository, inst, output)
					output.MarkAsFailed(nil, nil)
				} else {
					validateTrace.AppendDebugf("%v %v is already installed", packageName, targetVersion).End()
					output.MarkAsSucceeded()
				}
				if installState != localpackages.Installed && installState != localpackages.Unknown {
					repository.SetInstallState(tracer, packageName, instToCheck.Version(), localpackages.Installed)
				}

				validateTrace.End()
				return true
			}

			validateTrace.AppendInfo(validateOutput.GetStdout())
			validateTrace.AppendError(validateOutput.GetStderr())
			validateTrace.End()
		}
	}

	checkTrace.WithExitcode(1)
	return false
}

// selectService chooses the implementation of PackageService to use for a given execution of the plugin
func selectService(tracer trace.Tracer, serviceEndpoint string, localrepo localpackages.Repository) packageservice.PackageService {
	region, _ := platform.Region()
	appCfg, err := appconfig.Config(false)

	if (err == nil && appCfg.Birdwatcher.ForceEnable) || !ssms3.UseSSMS3Service(tracer, serviceEndpoint, region) {
		tracer.CurrentTrace().AppendInfof("S3 repository is not marked active in %v %v", region, serviceEndpoint)
		return birdwatcher.New(serviceEndpoint, localrepo)
	}

	tracer.CurrentTrace().AppendInfof("S3 repository is marked active")
	return ssms3.New(serviceEndpoint, region)
}

// Execute runs the plugin operation and returns output
func (p *Plugin) Execute(context context.T, config contracts.Configuration, cancelFlag task.CancelFlag, output iohandler.IOHandler) {
	p.execute(context, config, cancelFlag, output)
	return
}

func (p *Plugin) execute(context context.T, config contracts.Configuration, cancelFlag task.CancelFlag, output iohandler.IOHandler) {
	log := context.Log()
	log.Info("RunCommand started with configuration ", config)
	tracer := trace.NewTracer(log)
	defer tracer.BeginSection("configurePackage").End()

	out := trace.PluginOutputTrace{Tracer: tracer}

	if cancelFlag.ShutDown() {
		out.MarkAsShutdown()
	} else if cancelFlag.Canceled() {
		out.MarkAsCancelled()
	} else if input, err := parseAndValidateInput(config.Properties); err != nil {
		tracer.CurrentTrace().WithError(err).End()
		out.MarkAsFailed(nil, nil)
	} else {
		packageService := p.packageServiceSelector(tracer, input.Repository, p.localRepository)
		//Return failure if the manifest cannot be accessed
		//Return failure if the package version is installed, but the manifest is no longer available
		packageArn, manifestVersion, isSameAsCache, err := getPackageArnAndVersion(tracer, packageService, input)

		if err != nil {
			tracer.CurrentTrace().WithError(err).End()
			out.MarkAsFailed(nil, nil)
		} else if err := p.localRepository.LockPackage(tracer, packageArn, input.Action); err != nil {
			// do not allow multiple actions to be performed at the same time for the same package
			// this is possible with multiple concurrent runcommand documents
			tracer.CurrentTrace().WithError(err).End()
			out.MarkAsFailed(nil, nil)
		} else {
			defer p.localRepository.UnlockPackage(tracer, packageArn)

			log.Debugf("Prepare for %v %v %v", input.Action, input.Name, input.Version)
			inst, uninst, installState, installedVersion := prepareConfigurePackage(
				tracer,
				config,
				p.localRepository,
				packageService,
				input,
				packageArn,
				manifestVersion,
				isSameAsCache,
				&out)
			log.Debugf("HasInst %v, HasUninst %v, InstallState %v, PackageArn %v, InstalledVersion %v", inst != nil, uninst != nil, installState, packageArn, installedVersion)

			//if the status is already decided as failed or succeeded, do not execute anything
			if out.GetStatus() != contracts.ResultStatusFailed && out.GetStatus() != contracts.ResultStatusSuccess {
				alreadyInstalled := checkAlreadyInstalled(tracer, context, p.localRepository, installedVersion, installState, inst, uninst, &out)
				// if already failed or already installed and valid, do not execute install
				// if it is already installed and the cache is the same, do not execute install
				if !alreadyInstalled || !isSameAsCache {
					log.Debugf("Calling execute, current status %v", out.GetStatus())
					executeConfigurePackage(
						tracer,
						context,
						p.localRepository,
						inst,
						uninst,
						installState,
						&out)
				}
			}

			if err := p.localRepository.LoadTraces(tracer, packageArn); err != nil {
				log.Errorf("Error loading prior traces: %v", err.Error())
			}
			if out.GetStatus().IsReboot() {
				err := p.localRepository.PersistTraces(tracer, packageArn)
				if err != nil {
					log.Errorf("Error persisting traces: %v", err.Error())
				}
			} else {
				version := manifestVersion
				if input.Action == InstallAction {
					version = inst.Version()
				} else if input.Action == UninstallAction {
					version = uninst.Version()
				}

				startTime := tracer.Traces()[0].Start
				for _, trace := range tracer.Traces() {
					if trace.Start < startTime {
						startTime = trace.Start
					}
				}

				err := packageService.ReportResult(tracer, packageservice.PackageResult{
					Exitcode:               int64(out.GetExitCode()),
					Operation:              input.Action,
					PackageName:            input.Name,
					PreviousPackageVersion: installedVersion,
					Timing:                 startTime,
					Version:                version,
					Trace:                  packageservice.ConvertToPackageServiceTrace(tracer.Traces()),
				})
				if err != nil {
					out.AppendErrorf(log, "Error reporting results: %v", err.Error())
				}
			}
		}
	}

	output.SetExitCode(out.GetExitCode())
	output.SetStatus(out.GetStatus())

	// convert trace
	traceout := tracer.ToPluginOutput()
	output.AppendInfo(traceout.GetStdout())
	output.AppendError(traceout.GetStderr())

	return
}

// Name returns the name of the plugin.
func Name() string {
	return appconfig.PluginNameAwsConfigurePackage
}

func getPackageArnAndVersion(
	tracer trace.Tracer,
	packageService packageservice.PackageService,
	input *ConfigurePackagePluginInput) (string, string, bool, error) {

	//always download the manifest before acting upon the request
	trace := tracer.BeginSection("download manifest")

	version := input.Version
	if packageservice.IsLatest(input.Version) {
		version = packageservice.Latest
	}

	packageArn, version, isSameAsCache, err := packageService.DownloadManifest(tracer, input.Name, version)
	trace.AppendDebugf("got manifest for package %v version %v isSameAsCache %v", packageArn, version, isSameAsCache)

	if err != nil {
		trace.WithError(err).End()
		return "", "", false, err
	}
	trace.End()

	return packageArn, version, isSameAsCache, nil
}
