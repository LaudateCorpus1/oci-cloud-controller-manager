// Copyright 2017 Oracle and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package block

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ociprovider "github.com/oracle/oci-cloud-controller-manager/pkg/cloudprovider/providers/oci"
	"github.com/oracle/oci-cloud-controller-manager/pkg/flexvolume"
	"github.com/oracle/oci-cloud-controller-manager/pkg/oci/client"
	"github.com/oracle/oci-cloud-controller-manager/pkg/util/iscsi"
	"github.com/oracle/oci-go-sdk/core"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
)

const (
	// FIXME: Assume lun 1 for now?? Can we get the LUN via the API?
	diskIDByPathTemplate = "/dev/disk/by-path/ip-%s:%d-iscsi-%s-lun-1"
	volumeOCIDTemplate   = "ocid1.volume.oc1.%s.%s"
	ocidPrefix           = "ocid1."
)

// OCIFlexvolumeDriver implements the flexvolume.Driver interface for OCI.
type OCIFlexvolumeDriver struct {
	K      kubernetes.Interface
	master bool
	logger *zap.SugaredLogger
}

// NewOCIFlexvolumeDriver creates a new driver
func NewOCIFlexvolumeDriver(logger *zap.SugaredLogger) (fvd *OCIFlexvolumeDriver, err error) {

	defer func() {
		if e := recover(); e != nil {
			fvd = nil
			err = fmt.Errorf("%+v", e)
		}
	}()

	path := GetConfigPath()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		k, err := constructKubeClient()
		if err != nil {
			return nil, err
		}
		return &OCIFlexvolumeDriver{K: k, master: true, logger: logger}, nil
	} else if os.IsNotExist(err) {
		logger.With(zap.Error(err), "path", path).Error("Config file does not exist. Assuming worker node.")
		return &OCIFlexvolumeDriver{logger: logger}, nil
	}
	return nil, err
}

// GetDriverDirectory gets the ath for the flexvolume driver either from the
// env or default.
func GetDriverDirectory() string {
	// TODO(apryde): Document this ENV var.
	path := os.Getenv("OCI_FLEXD_DRIVER_DIRECTORY")
	if path == "" {
		path = "/usr/libexec/kubernetes/kubelet-plugins/volume/exec/oracle~oci"
	}
	return path
}

// GetConfigDirectory gets the path to where config files are stored.
func GetConfigDirectory() string {
	path := os.Getenv("OCI_FLEXD_CONFIG_DIRECTORY")
	if path != "" {
		return path
	}

	return GetDriverDirectory()
}

// GetConfigPath gets the path to the OCI API credentials.
func GetConfigPath() string {
	path := GetConfigDirectory()
	return filepath.Join(path, "config.yaml")
}

// GetKubeconfigPath gets the override path of the 'kubeconfig'. This override
// can be uses to explicitly set the name and location of the kubeconfig file
// via the OCI_FLEXD_KUBECONFIG_PATH environment variable. If this value is not
// specified then the default GetConfigDirectory mechanism is used.
func GetKubeconfigPath() string {
	kcp := os.Getenv("OCI_FLEXD_KUBECONFIG_PATH")
	if kcp == "" {
		kcp = fmt.Sprintf("%s/%s", strings.TrimRight(GetConfigDirectory(), "/"), "kubeconfig")
	}
	return kcp
}

const (
	rateLimitQPSDefault    = 20.0
	rateLimitBucketDefault = 5
)

func newClient(config *Config) (client.Interface, error) {
	cp, err := configurationProviderFromConfig(config)
	if err != nil {
		return nil, err
	}

	return client.New(zap.New(nil).Sugar(), cp, &client.RateLimiter{
		Reader: flowcontrol.NewTokenBucketRateLimiter(
			rateLimitQPSDefault,
			rateLimitBucketDefault,
		),
		Writer: flowcontrol.NewTokenBucketRateLimiter(
			rateLimitQPSDefault,
			rateLimitBucketDefault,
		),
	})
}

// Init checks that we have the appropriate credentials and metadata API access
// on driver initialisation.
func (d OCIFlexvolumeDriver) Init() flexvolume.DriverStatus {
	path := GetConfigPath()
	if d.master {
		config, err := ConfigFromFile(path)
		if err != nil {
			return flexvolume.Fail(d.logger, err)
		}
		_, err = newClient(config)
		if err != nil {
			return flexvolume.Fail(d.logger, err)
		}

		_, err = constructKubeClient()
		if err != nil {
			return flexvolume.Fail(d.logger, err)
		}
	} else {
		d.logger.Info("Assuming worker node.")
	}

	return flexvolume.Succeed(d.logger)
}

// deriveVolumeOCID will figure out the correct OCID for a volume
// based solely on the region key and volumeName. Because of differences
// across regions we need to impose some awkward logic here to get the correct
// OCID or if it is already an OCID then return the OCID.
func deriveVolumeOCID(regionKey string, volumeName string) string {
	if strings.HasPrefix(volumeName, ocidPrefix) {
		return volumeName
	}

	var volumeOCID string
	if regionKey == "fra" {
		volumeOCID = fmt.Sprintf(volumeOCIDTemplate, "eu-frankfurt-1", volumeName)
	} else {
		volumeOCID = fmt.Sprintf(volumeOCIDTemplate, regionKey, volumeName)
	}

	return volumeOCID
}

// constructKubeClient uses a kubeconfig layed down by a secret via deploy.sh to return
// a kube clientset.
func constructKubeClient() (*kubernetes.Clientset, error) {
	c, err := clientcmd.BuildConfigFromFlags("", GetKubeconfigPath())
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(c)
}

// lookupNodeID returns the OCID for the given nodeName.
func lookupNodeID(k kubernetes.Interface, nodeName string) (string, error) {
	n, err := k.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if n.Spec.ProviderID == "" {
		return "", errors.New("node is missing provider id")
	}
	return n.Spec.ProviderID, nil
}

// Attach initiates the attachment of the given OCI volume to the k8s worker
// node.
func (d OCIFlexvolumeDriver) Attach(opts flexvolume.Options, nodeName string) flexvolume.DriverStatus {
	config, err := ConfigFromFile(GetConfigPath())
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	c, err := newClient(config)
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	id, err := lookupNodeID(d.K, nodeName)
	if err != nil {
		return flexvolume.Fail(d.logger, "failed to look up node id: ", err)
	}

	// Handle possible oci:// prefix.
	id, err = ociprovider.MapProviderIDToInstanceID(id)
	if err != nil {
		return flexvolume.Fail(d.logger, "failed to map nodes provider id to instance id: ", err)
	}

	ctx := context.Background()

	instance, err := c.Compute().GetInstance(ctx, id)
	if err != nil {
		return flexvolume.Fail(d.logger, "failed to get instance: ", err)
	}

	volumeOCID := deriveVolumeOCID(config.Auth.RegionKey, opts["kubernetes.io/pvOrVolumeName"])

	d.logger.With("volumeID", volumeOCID, "instanceID", *instance.Id).Info("Attaching volume to instance")

	attachment, err := c.Compute().AttachVolume(ctx, *instance.Id, volumeOCID)
	if err != nil {
		if !client.IsConflict(err) {
			return flexvolume.Fail(d.logger, "failed to attach volume: ", err)
		}
		// If we get a 409 conflict response when attaching we
		// presume that the device is already attached.
		d.logger.With("volumeID", volumeOCID).Info("Attach(): Volume already attached.")
		attachment, err = c.Compute().FindVolumeAttachment(ctx, config.Auth.CompartmentID, volumeOCID)
		if err != nil {
			return flexvolume.Fail(d.logger, "failed to find volume attachment: ", err)
		}
		if *attachment.GetInstanceId() != *instance.Id {
			return flexvolume.Fail(d.logger, "Already attached to anoter instance: ", *instance.Id)
		}
	}

	attachment, err = c.Compute().WaitForVolumeAttached(ctx, *attachment.GetId())
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	d.logger.With("attachmentID", *attachment.GetId()).Info("attachment made")
	iscsiAttachment, ok := attachment.(core.IScsiVolumeAttachment)
	if !ok {
		return flexvolume.Fail(d.logger, "Only ISCSI volume attachments are currently supported")
	}

	return flexvolume.DriverStatus{
		Status: flexvolume.StatusSuccess,
		Device: fmt.Sprintf(diskIDByPathTemplate, *iscsiAttachment.Ipv4, *iscsiAttachment.Port, *iscsiAttachment.Iqn),
	}
}

// Detach detaches the volume from the worker node.
func (d OCIFlexvolumeDriver) Detach(pvOrVolumeName, nodeName string) flexvolume.DriverStatus {
	config, err := ConfigFromFile(GetConfigPath())
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}
	c, err := newClient(config)
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	volumeOCID := deriveVolumeOCID(config.Auth.RegionKey, pvOrVolumeName)
	ctx := context.Background()
	attachment, err := c.Compute().FindVolumeAttachment(ctx, config.Auth.CompartmentID, volumeOCID)
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	err = c.Compute().DetachVolume(ctx, *attachment.GetId())
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	err = c.Compute().WaitForVolumeDetached(ctx, *attachment.GetId())
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}
	return flexvolume.Succeed(d.logger)
}

// WaitForAttach searches for the the volume attachment created by Attach() and
// waits for its life cycle state to reach ATTACHED.
func (d OCIFlexvolumeDriver) WaitForAttach(mountDevice string, _ flexvolume.Options) flexvolume.DriverStatus {
	return flexvolume.DriverStatus{
		Status: flexvolume.StatusSuccess,
		Device: mountDevice,
	}
}

// IsAttached checks whether the volume is attached to the host.
// TODO(apryde): The documentation states that this is called from the Kubelet
// and KCM. Implementation requries credentials which won't be present on nodes
// but I've only ever seen it called by the KCM.
func (d OCIFlexvolumeDriver) IsAttached(opts flexvolume.Options, nodeName string) flexvolume.DriverStatus {
	config, err := ConfigFromFile(GetConfigPath())
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}
	c, err := newClient(config)
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	ctx := context.Background()
	volumeOCID := deriveVolumeOCID(config.Auth.RegionKey, opts["kubernetes.io/pvOrVolumeName"])
	attachment, err := c.Compute().FindVolumeAttachment(ctx, config.Auth.CompartmentID, volumeOCID)
	if err != nil {
		return flexvolume.DriverStatus{
			Status:   flexvolume.StatusSuccess,
			Message:  err.Error(),
			Attached: false,
		}
	}

	d.logger.With("attachmentID", *attachment.GetId()).Info("attach: found volume attachment")

	return flexvolume.DriverStatus{
		Status:   flexvolume.StatusSuccess,
		Attached: true,
	}
}

// MountDevice connects the iSCSI target on the k8s worker node before mounting
// and (if necessary) formatting the disk.
func (d OCIFlexvolumeDriver) MountDevice(mountDir, mountDevice string, opts flexvolume.Options) flexvolume.DriverStatus {
	iSCSIMounter, err := iscsi.NewFromDevicePath(d.logger, mountDevice)
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	if isMounted, oErr := iSCSIMounter.DeviceOpened(mountDevice); oErr != nil {
		return flexvolume.Fail(d.logger, oErr)
	} else if isMounted {
		return flexvolume.Succeed(d.logger, "Device already mounted. Nothing to do.")
	}

	if err = iSCSIMounter.AddToDB(); err != nil {
		return flexvolume.Fail(d.logger, err)
	}
	if err = iSCSIMounter.SetAutomaticLogin(); err != nil {
		return flexvolume.Fail(d.logger, err)
	}
	if err = iSCSIMounter.Login(); err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	if !waitForPathToExist(mountDevice, 20) {
		return flexvolume.Fail(d.logger, "Failed waiting for device to exist: ", mountDevice)
	}

	options := []string{}
	if opts[flexvolume.OptionReadWrite] == "ro" {
		options = []string{"ro"}
	}
	err = iSCSIMounter.FormatAndMount(mountDevice, mountDir, opts[flexvolume.OptionFSType], options)
	if err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	return flexvolume.Succeed(d.logger)
}

// UnmountDevice unmounts the disk, logs out the iscsi target, and deletes the
// iscsi node record.
func (d OCIFlexvolumeDriver) UnmountDevice(mountPath string) flexvolume.DriverStatus {
	iSCSIMounter, err := iscsi.NewFromMountPointPath(d.logger, mountPath)
	if err != nil {
		if err == iscsi.ErrMountPointNotFound {
			return flexvolume.Succeed(d.logger, "Mount point not found. Nothing to do.")
		}
		return flexvolume.Fail(d.logger, err)
	}

	if err = iSCSIMounter.UnmountPath(mountPath); err != nil {
		return flexvolume.Fail(d.logger, err)
	}
	if err = iSCSIMounter.Logout(); err != nil {
		return flexvolume.Fail(d.logger, err)
	}
	if err = iSCSIMounter.RemoveFromDB(); err != nil {
		return flexvolume.Fail(d.logger, err)
	}

	return flexvolume.Succeed(d.logger)
}

// Mount is unimplemented as we use the --enable-controller-attach-detach flow
// and as such mount the drive in MountDevice().
func (d OCIFlexvolumeDriver) Mount(mountDir string, opts flexvolume.Options) flexvolume.DriverStatus {
	return flexvolume.NotSupported(d.logger)
}

// Unmount is unimplemented as we use the --enable-controller-attach-detach flow
// and as such unmount the drive in UnmountDevice().
func (d OCIFlexvolumeDriver) Unmount(mountDir string) flexvolume.DriverStatus {
	return flexvolume.NotSupported(d.logger)
}