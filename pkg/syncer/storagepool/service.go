/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storagepool

import (
	"context"
	"reflect"
	"sync"
	"time"

	cnstypes "github.com/vmware/govmomi/cns/types"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	spv1alpha1 "sigs.k8s.io/vsphere-csi-driver/pkg/apis/storagepool/cns/v1alpha1"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common/commonco"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common/commonco/k8sorchestrator"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	commontypes "sigs.k8s.io/vsphere-csi-driver/pkg/syncer/types"
)

// Service holds the controllers needed to manage StoragePools
type Service struct {
	spController   *SpController
	scWatchCntlr   *StorageClassWatch
	migrationCntlr *migrationController
	clusterID      string
}

var (
	defaultStoragePoolService     *Service = new(Service)
	defaultStoragePoolServiceLock sync.Mutex
)

// InitStoragePoolService initializes the StoragePool service that updates
// vSphere Datastore information into corresponding k8s StoragePool resources.
func InitStoragePoolService(ctx context.Context, configInfo *commontypes.ConfigInfo) error {
	log := logger.GetLogger(ctx)
	log.Infof("Initializing Storage Pool Service")

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Errorf("Failed to get Kubernetes config. Err: %+v", err)
		return err
	}

	apiextensionsClientSet, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		log.Errorf("Failed to create Kubernetes client using config. Err: %+v", err)
		return err
	}

	// Create StoragePool CRD
	crdKind := reflect.TypeOf(spv1alpha1.StoragePool{}).Name()
	err = createCustomResourceDefinition(ctx, apiextensionsClientSet, "storagepools", crdKind)
	if err != nil {
		log.Errorf("Failed to create %q CRD. Err: %+v", crdKind, err)
		return err
	}

	// Get VC connection
	vc, err := commontypes.GetVirtualCenterInstance(ctx, configInfo)
	if err != nil {
		log.Errorf("Failed to get vCenter from ConfigInfo. Err: %+v", err)
		return err
	}

	err = vc.ConnectPbm(ctx)
	if err != nil {
		log.Errorf("Failed to connect to SPBM service. Err: %+v", err)
		return err
	}

	// Start the services
	spController, err := newSPController(vc, configInfo.Cfg.Global.ClusterID)
	if err != nil {
		log.Errorf("Failed starting StoragePool controller. Err: %+v", err)
		return err
	}

	scWatchCntlr, err := startStorageClassWatch(ctx, spController, cfg)
	if err != nil {
		log.Errorf("Failed starting the Storageclass watch. Err: %+v", err)
		return err
	}

	migrationController := initMigrationController(vc, configInfo.Cfg.Global.ClusterID)
	go func() {
		diskDecommEnablementTicker := time.NewTicker(common.DefaultFeatureEnablementCheckInterval)
		defer diskDecommEnablementTicker.Stop()
		clusterFlavor := cnstypes.CnsClusterFlavorWorkload
		for ; true; <-diskDecommEnablementTicker.C {
			k8sInitParams := k8sorchestrator.K8sSupervisorInitParams{
				SupervisorFeatureStatesConfigInfo: configInfo.Cfg.FeatureStatesConfig,
			}
			coCommonInterface, _ := commonco.GetContainerOrchestratorInterface(ctx, common.Kubernetes, clusterFlavor, k8sInitParams)
			if !coCommonInterface.IsFSSEnabled(ctx, common.VSANDirectDiskDecommission) {
				log.Infof("VSANDirectDiskDecommission feature is disabled on the cluster")
			} else {
				_, err := initDiskDecommController(ctx, migrationController)
				if err != nil {
					log.Warnf("Error while initializing disk decommission controller. Error: %+v. Retry will be triggered at %v", err, time.Now().Add(common.DefaultFeatureEnablementCheckInterval))
					continue
				}
				break
			}
		}
	}()

	// Create the default Service
	defaultStoragePoolServiceLock.Lock()
	defer defaultStoragePoolServiceLock.Unlock()
	defaultStoragePoolService.spController = spController
	defaultStoragePoolService.scWatchCntlr = scWatchCntlr
	defaultStoragePoolService.migrationCntlr = migrationController
	defaultStoragePoolService.clusterID = configInfo.Cfg.Global.ClusterID

	startPropertyCollectorListener(ctx)

	log.Infof("Done initializing Storage Pool Service")
	return nil
}

// GetStoragePoolService returns the single instance of Service
func GetStoragePoolService() *Service {
	return defaultStoragePoolService
}

// GetScWatch returns the active StorageClassWatch initialized in this service
func (sps *Service) GetScWatch() *StorageClassWatch {
	return sps.scWatchCntlr
}

// GetSPController returns the single SpController intialized in this service
func (sps *Service) GetSPController() *SpController {
	return sps.spController
}

// ResetVC will be called whenever the connection to vCenter is recycled. This will renew the PropertyCollector
// listener of StoragePool as well as update the controllers with the new refreshed VC connection.
func ResetVC(ctx context.Context, vc *cnsvsphere.VirtualCenter) {
	log := logger.GetLogger(ctx)
	if vc == nil {
		log.Errorf("VirtualCenter not given to Reset")
		return
	}
	err := vc.ConnectPbm(ctx)
	if err != nil {
		log.Errorf("Failed to connect to SPBM service. Err: %+v", err)
		return
	}
	log.Infof("Resetting VC connection in StoragePool service")
	defaultStoragePoolServiceLock.Lock()
	defer defaultStoragePoolServiceLock.Unlock()

	defaultStoragePoolService.spController.vc = vc
	defaultStoragePoolService.scWatchCntlr.vc = vc
	defaultStoragePoolService.migrationCntlr.vc = vc
	// PC listener will automatically reestablish its session with VC
	log.Debugf("Successfully reset VC connection in StoragePool service")
}
