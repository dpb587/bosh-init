package cmd

import (
	bihttpagent "github.com/cloudfoundry/bosh-agent/agentclient/http"
	biblobstore "github.com/cloudfoundry/bosh-init/blobstore"
	bicloud "github.com/cloudfoundry/bosh-init/cloud"
	biconfig "github.com/cloudfoundry/bosh-init/config"
	bicpirel "github.com/cloudfoundry/bosh-init/cpi/release"
	bidepl "github.com/cloudfoundry/bosh-init/deployment"
	biinstall "github.com/cloudfoundry/bosh-init/installation"
	biinstallmanifest "github.com/cloudfoundry/bosh-init/installation/manifest"
	birel "github.com/cloudfoundry/bosh-init/release"
	birelsetmanifest "github.com/cloudfoundry/bosh-init/release/set/manifest"
	biui "github.com/cloudfoundry/bosh-init/ui"
	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	bihttpclient "github.com/cloudfoundry/bosh-utils/httpclient"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
)

type DeploymentDeleter interface {
	DeleteDeployment(stage biui.Stage) (err error)
}

func NewDeploymentDeleter(
	ui biui.UI,
	logTag string,
	logger boshlog.Logger,
	deploymentStateService biconfig.DeploymentStateService,
	releaseManager birel.Manager,
	cloudFactory bicloud.Factory,
	agentClientFactory bihttpagent.AgentClientFactory,
	blobstoreFactory biblobstore.Factory,
	deploymentManagerFactory bidepl.ManagerFactory,
	deploymentManifestPath string,
	cpiInstaller bicpirel.CpiInstaller,
	cpiUninstaller biinstall.Uninstaller,
	releaseFetcher birel.Fetcher,
	releaseSetAndInstallationManifestParser ReleaseSetAndInstallationManifestParser,
	tempRootConfigurator TempRootConfigurator,
	targetProvider biinstall.TargetProvider,
) DeploymentDeleter {
	return &deploymentDeleter{
		ui:                                      ui,
		logTag:                                  logTag,
		logger:                                  logger,
		deploymentStateService:                  deploymentStateService,
		releaseManager:                          releaseManager,
		cloudFactory:                            cloudFactory,
		agentClientFactory:                      agentClientFactory,
		blobstoreFactory:                        blobstoreFactory,
		deploymentManagerFactory:                deploymentManagerFactory,
		deploymentManifestPath:                  deploymentManifestPath,
		cpiInstaller:                            cpiInstaller,
		cpiUninstaller:                          cpiUninstaller,
		releaseFetcher:                          releaseFetcher,
		releaseSetAndInstallationManifestParser: releaseSetAndInstallationManifestParser,
		tempRootConfigurator:                    tempRootConfigurator,
		targetProvider:                          targetProvider,
	}
}

type deploymentDeleter struct {
	ui                                      biui.UI
	logTag                                  string
	logger                                  boshlog.Logger
	deploymentStateService                  biconfig.DeploymentStateService
	releaseManager                          birel.Manager
	cloudFactory                            bicloud.Factory
	agentClientFactory                      bihttpagent.AgentClientFactory
	blobstoreFactory                        biblobstore.Factory
	deploymentManagerFactory                bidepl.ManagerFactory
	deploymentManifestPath                  string
	cpiInstaller                            bicpirel.CpiInstaller
	cpiUninstaller                          biinstall.Uninstaller
	releaseFetcher                          birel.Fetcher
	releaseSetAndInstallationManifestParser ReleaseSetAndInstallationManifestParser
	tempRootConfigurator                    TempRootConfigurator
	targetProvider                          biinstall.TargetProvider
}

func (c *deploymentDeleter) DeleteDeployment(stage biui.Stage) (err error) {
	c.ui.PrintLinef("Deployment state: '%s'", c.deploymentStateService.Path())

	if !c.deploymentStateService.Exists() {
		c.ui.PrintLinef("No deployment state file found.")
		return nil
	}

	deploymentState, err := c.deploymentStateService.Load()
	if err != nil {
		return bosherr.WrapError(err, "Loading deployment state")
	}

	target, err := c.targetProvider.NewTarget()
	if err != nil {
		return bosherr.WrapError(err, "Determining installation target")
	}

	err = c.tempRootConfigurator.PrepareAndSetTempRoot(target.TmpPath(), c.logger)
	if err != nil {
		return bosherr.WrapError(err, "Setting temp root")
	}

	defer func() {
		err := c.releaseManager.DeleteAll()
		if err != nil {
			c.logger.Warn(c.logTag, "Deleting all extracted releases: %s", err.Error())
		}
	}()

	var installationManifest biinstallmanifest.Manifest
	err = stage.PerformComplex("validating", func(stage biui.Stage) error {
		var releaseSetManifest birelsetmanifest.Manifest
		releaseSetManifest, installationManifest, err = c.releaseSetAndInstallationManifestParser.ReleaseSetAndInstallationManifest(c.deploymentManifestPath)
		if err != nil {
			return err
		}

		cpiReleaseName := installationManifest.Template.Release
		cpiReleaseRef, found := releaseSetManifest.FindByName(cpiReleaseName)
		if !found {
			return bosherr.Errorf("installation release '%s' must refer to a release in releases", cpiReleaseName)
		}

		err = c.releaseFetcher.DownloadAndExtract(cpiReleaseRef, stage)
		if err != nil {
			return err
		}

		err = c.cpiInstaller.ValidateCpiRelease(installationManifest, stage)
		return err
	})
	if err != nil {
		return err
	}

	err = c.cpiInstaller.WithInstalledCpiRelease(installationManifest, target, stage, func(localCpiInstallation biinstall.Installation) error {
		return localCpiInstallation.WithRunningRegistry(c.logger, stage, func() error {
			err = c.findAndDeleteDeployment(stage, localCpiInstallation, deploymentState.DirectorID, installationManifest.Mbus)

			if err != nil {
				return err
			}

			return stage.Perform("Uninstalling local artifacts for CPI and deployment", func() error {
				err := c.cpiUninstaller.Uninstall(localCpiInstallation.Target())
				if err != nil {
					return err
				}

				return c.deploymentStateService.Cleanup()
			})
		})
	})

	return err
}

func (c *deploymentDeleter) findAndDeleteDeployment(stage biui.Stage, installation biinstall.Installation, directorID, installationMbus string) error {
	deploymentManager, err := c.deploymentManager(installation, directorID, installationMbus)
	if err != nil {
		return err
	}
	err = c.findCurrentDeploymentAndDelete(stage, deploymentManager)
	if err != nil {
		return bosherr.WrapError(err, "Deleting deployment")
	}
	return deploymentManager.Cleanup(stage)
}

func (c *deploymentDeleter) findCurrentDeploymentAndDelete(stage biui.Stage, deploymentManager bidepl.Manager) error {
	c.logger.Debug(c.logTag, "Finding current deployment...")
	deployment, found, err := deploymentManager.FindCurrent()
	if err != nil {
		return bosherr.WrapError(err, "Finding current deployment")
	}

	return stage.PerformComplex("deleting deployment", func(deleteStage biui.Stage) error {
		if !found {
			//TODO: skip? would require adding skip support to PerformComplex
			c.logger.Debug(c.logTag, "No current deployment found...")
			return nil
		}

		return deployment.Delete(deleteStage)
	})
}

func (c *deploymentDeleter) deploymentManager(installation biinstall.Installation, directorID, installationMbus string) (bidepl.Manager, error) {
	c.logger.Debug(c.logTag, "Creating cloud client...")
	cloud, err := c.cloudFactory.NewCloud(installation, directorID)
	if err != nil {
		return nil, bosherr.WrapError(err, "Creating CPI client from CPI installation")
	}

	c.logger.Debug(c.logTag, "Creating agent client...")
	agentClient := c.agentClientFactory.NewAgentClient(directorID, installationMbus)

	c.logger.Debug(c.logTag, "Creating blobstore client...")

	blobstore, err := c.blobstoreFactory.Create(installationMbus, bihttpclient.CreateDefaultClientInsecureSkipVerify())
	if err != nil {
		return nil, bosherr.WrapError(err, "Creating blobstore client")
	}

	c.logger.Debug(c.logTag, "Creating deployment manager...")
	return c.deploymentManagerFactory.NewManager(cloud, agentClient, blobstore), nil
}
