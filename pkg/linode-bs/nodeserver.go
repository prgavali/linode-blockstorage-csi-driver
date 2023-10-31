package linodebs

/*
Copyright 2018 The Kubernetes Authors.

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

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/glog"
	"github.com/linode/linode-blockstorage-csi-driver/pkg/common"
	linodeclient "github.com/linode/linode-blockstorage-csi-driver/pkg/linode-client"
	"github.com/linode/linode-blockstorage-csi-driver/pkg/metadata"
	mountmanager "github.com/linode/linode-blockstorage-csi-driver/pkg/mount-manager"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/utils/mount"
)

const (
	maxVolumesPerNode = 8
)

type LinodeNodeServer struct {
	Driver          *LinodeDriver
	Mounter         *mount.SafeFormatAndMount
	DeviceUtils     mountmanager.DeviceUtils
	CloudProvider   linodeclient.LinodeClient
	MetadataService metadata.MetadataService
	// TODO: Only lock mutually exclusive calls and make locking more fine grained
	mux sync.Mutex
}

var _ csi.NodeServer = &LinodeNodeServer{}

func (ns *LinodeNodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	ns.mux.Lock()
	defer ns.mux.Unlock()
	glog.V(4).Infof("NodePublishVolume called with req: %#v", req)

	// Validate Arguments
	targetPath := req.GetTargetPath()
	stagingTargetPath := req.GetStagingTargetPath()
	readOnly := req.GetReadonly()
	volumeID := req.GetVolumeId()
	volumeCapability := req.GetVolumeCapability()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Staging Target Path must be provided")
	}
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Target Path must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Volume Capability must be provided")
	}

	notMnt, err := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
	if err != nil && !os.IsNotExist(err) {
		glog.Errorf("cannot validate mount point: %s %v", targetPath, err)
		return nil, err
	}
	if !notMnt {
		// TODO(#95): check if mount is compatible. Return OK if it is, or appropriate error.
		/*
			1) Target Path MUST be the vol referenced by vol ID
			2) VolumeCapability MUST match
			3) Readonly MUST match

		*/
		return &csi.NodePublishVolumeResponse{}, nil
	}
	// Perform a bind mount to the full path to allow duplicate mounts of the same PD.
	options := []string{"bind"}
	if readOnly {
		options = append(options, "ro")
	}

	if blk := volumeCapability.GetBlock(); blk != nil {
		respBlock, errBlock := ns.processMountForBlock(req, stagingTargetPath, targetPath, volumeID, options)
		return respBlock, errBlock
	} else {

		if err := os.MkdirAll(targetPath, os.FileMode(0755)); err != nil {
			glog.Errorf("mkdir failed on disk %s (%v)", targetPath, err)
			return nil, err
		}

		err = ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, "ext4", options)
		if err != nil {
			notMnt, mntErr := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
			if mntErr != nil {
				glog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr)
				return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to check whether target path is a mount point: %v", err))
			}
			if !notMnt {
				if mntErr = ns.Mounter.Interface.Unmount(targetPath); mntErr != nil {
					glog.Errorf("Failed to unmount: %v", mntErr)
					return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to unmount target path: %v", err))
				}
				notMnt, mntErr := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
				if mntErr != nil {
					glog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr)
					return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to check whether target path is a mount point: %v", err))
				}
				if !notMnt {
					// This is very odd, we don't expect it.  We'll try again next sync loop.
					glog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", targetPath)
					return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume something is wrong with mounting: %v", err))
				}
			}
			os.Remove(targetPath)
			glog.Errorf("Mount of disk %s failed: %v", targetPath, err)
			return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume mount of disk failed: %v", err))
		}

		glog.V(4).Infof("Successfully mounted %s", targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}
}

func getMountSources(target string) ([]string, error) {
	_, err := exec.LookPath("findmnt")
	if err != nil {
		if err == exec.ErrNotFound {
			return nil, fmt.Errorf("%q executable not found in $PATH", "findmnt")
		}
		return nil, err
	}
	out, err := exec.Command("sh", "-c", fmt.Sprintf("findmnt -o SOURCE -n -M %s", target)).CombinedOutput()
	if err != nil {
		// findmnt exits with non zero exit status if it couldn't find anything
		if strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("checking mounted failed: %v cmd: %q output: %q",
			err, "findmnt", string(out))
	}
	return strings.Split(string(out), "\n"), nil
}

func (ns *LinodeNodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	ns.mux.Lock()
	defer ns.mux.Unlock()
	// Validate Arguments
	targetPath := req.GetTargetPath()
	volID := req.GetVolumeId()
	if len(volID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Volume ID must be provided")
	}
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Target Path must be provided")
	}

	err := mount.CleanupMountPoint(targetPath, ns.Mounter.Interface, false /* bind mount */)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Unmount failed: %v\nUnmounting arguments: %s\n", err, targetPath))
	}

	glog.V(4).Infof("NodeUnpublishVolume called with args: %v, targetPath ", req, targetPath)

	if err := closeMountSources(targetPath); err != nil {
		return nil, err
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *LinodeNodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	ns.mux.Lock()
	defer ns.mux.Unlock()
	glog.V(4).Infof("NodeStageVolume called with req: %#v", req)

	// Validate Arguments
	volumeKey := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	volumeCapability := req.GetVolumeCapability()
	if len(volumeKey) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Staging Target Path must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Volume Capability must be provided")
	}

	key, err := common.ParseLinodeVolumeKey(volumeKey)
	if err != nil {
		return nil, err
	}

	// Part 1: Get device path of attached device
	partition := ""

	if part, ok := req.GetVolumeContext()["partition"]; ok {
		partition = part
	}

	deviceName := key.GetNormalizedLabel()
	devicePaths := ns.DeviceUtils.GetDiskByIdPaths(deviceName, partition)
	devicePath, err := ns.DeviceUtils.VerifyDevicePath(devicePaths)

	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Error verifying Linode Volume (%q) is attached: %v", key.GetVolumeLabel(), err))
	}
	if devicePath == "" {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Unable to find device path out of attempted paths: %v", devicePaths))
	}

	glog.V(4).Infof("Successfully found attached Linode Volume %q at device path %s.", deviceName, devicePath)

	// Part 2: Check if mount already exists at targetpath
	notMnt, err := ns.Mounter.Interface.IsLikelyNotMountPoint(stagingTargetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(stagingTargetPath, os.FileMode(0755)); err != nil {
				return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to create directory (%q): %v", stagingTargetPath, err))
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Unknown error when checking mount point (%q): %v", stagingTargetPath, err))
		}
	}

	if !notMnt {
		// TODO(#95): Check who is mounted here. No error if its us
		/*
			1) Target Path MUST be the vol referenced by vol ID
			2) VolumeCapability MUST match
			3) Readonly MUST match

		*/
		return &csi.NodeStageVolumeResponse{}, nil

	}

	// Part 3: Mount device to stagingTargetPath
	// Default fstype is ext4
	fstype := "ext4"
	options := []string{}
	if mnt := volumeCapability.GetMount(); mnt != nil {
		if mnt.FsType != "" {
			fstype = mnt.FsType
		}
		options = append(options, mnt.MountFlags...)
	} else if blk := volumeCapability.GetBlock(); blk != nil {
		glog.V(4).Info("Since it is a block device request, return raw device without format and mount", "devicePath", devicePath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	fmtAndMountSource := devicePath
	luksContext := getLuksContext(req.Secrets, req.VolumeContext, VolumeLifecycleNodeStageVolume)
	if luksContext.EncryptionEnabled {
		glog.V(4).Info("luksContext encryption enabled")
		formatted, err := blkidValid(devicePath)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to validate blkid (%q): %v", devicePath, err))
		}
		if !formatted {
			glog.V(4).Info("luks volume now formatting: ", devicePath)
			if err := luksContext.validate(); err != nil {
				return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to luks format validation (%q): %v", devicePath, err))
			}
			if err := luksFormat(devicePath, luksContext); err != nil {
				return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to luks format (%q): %v", devicePath, err))
			}
		}

		luksSource, err := luksPrepareMount(devicePath, luksContext)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to prepare luks mount (%q): %v", devicePath, err))
		}
		fmtAndMountSource = luksSource
	}

	glog.V(4).Info("formatting and mounting the drive")
	if err := ns.Mounter.FormatAndMount(fmtAndMountSource, stagingTargetPath, fstype, options); err != nil {
		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Failed to format and mount device from (%q) to (%q) with fstype (%q) and options (%q): %v",
				devicePath, stagingTargetPath, fstype, options, err))
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *LinodeNodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	ns.mux.Lock()
	defer ns.mux.Unlock()
	glog.V(4).Infof("NodeUnstageVolume called with req: %#v", req)
	// Validate arguments
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Staging Target Path must be provided")
	}

	err := mount.CleanupMountPoint(stagingTargetPath, ns.Mounter.Interface, false /* bind mount */)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("NodeUnstageVolume failed to unmount at path %s: %v", stagingTargetPath, err))
	}

	if err := closeMountSources(stagingTargetPath); err != nil {
		return nil, err
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func closeMountSources(path string) error {
	mountSources, err := getMountSources(path)
	if err != nil {
		return status.Error(codes.Internal, fmt.Sprintf("closeMountSources failed to to get mount sources %s: %v", path, err))
	}
	glog.V(4).Info("closing mount sources: ", mountSources)
	for _, source := range mountSources {
		isLuksMapping, mappingName, err := isLuksMapping(source)
		if err != nil {
			return status.Error(codes.Internal, fmt.Sprintf("closeMountSources failed determine if mount is a luks mapping %s: %v", path, err))
		}
		if isLuksMapping {
			glog.V(4).Infof("luksClose ", mappingName)
			if err := luksClose(mappingName); err != nil {
				return status.Error(codes.Internal, fmt.Sprintf("closeMountSources failed to close luks mount %s: %v", path, err))
			}
		}
	}

	return nil
}

// This will handle raw block volume mounts
// Incase of RAW volume mount, the Target will be devicefilepath  and NOT a mount directory.
// The mountType is "bind" mount and will not specify any FORMAT(e.g ext4, ext3..)
// e.g SOURCE (volume provider attached device on Host): /dev/xvde
// e.g TARGET (SoftLink to User defined POD device /dev/sda) : "/var/data/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/pvc-9b82dced-fcd6-4181-968e-ae269e0f2311"
func (ns *LinodeNodeServer) processMountForBlock(req *csi.NodePublishVolumeRequest, stagingTargetPath, target, volumeID string, options []string) (*csi.NodePublishVolumeResponse, error) {
	glog.V(4).Info("processMountForBlock volumeID(%s), target(%s), options(%s)", volumeID, target, options)

	key, err := common.ParseLinodeVolumeKey(volumeID)
	if err != nil {
		return nil, err
	}

	// Part 1: Get device path of attached device
	partition := ""

	if part, ok := req.GetVolumeContext()["partition"]; ok {
		partition = part
	}

	deviceName := key.GetNormalizedLabel()
	devicePaths := ns.DeviceUtils.GetDiskByIdPaths(deviceName, partition)
	devicePath, err := ns.DeviceUtils.VerifyDevicePath(devicePaths)

	targetDir := filepath.Dir(target)
	exists, err := mount.PathExists(targetDir)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Target Path Check failed %v", err))
	}

	if !exists {
		if err := MakeDir(targetDir); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Target Path creation failed  %v", err))
		}
	}

	// Create the mount point as a file since bind mount device node requires it to be a file
	glog.V(4).Info("Making target file (%s)", target)
	err = MakeFile(target)
	if err != nil {
		if removeErr := os.Remove(target); removeErr != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Remove mount target(%s) failed  %v", target, err))
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("Create mount target(%s) failed  %v", target, err))
	}

	glog.V(4).Info("Mounting source (%s)  to target (%s)", devicePath, target)
	if err := ns.Mounter.Mount(devicePath, target, "", options); err != nil {
		if removeErr := os.Remove(target); removeErr != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Remove mount target(%s) failed  %v", target, err))
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("Mount failed source(%s) target(%s) failed  %v", devicePath, target, err))
	}

	glog.V(4).Info("Block volume mounted successfully source (%s)  to target (%s)", devicePath, target)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *LinodeNodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	glog.V(4).Infof("NodeExpandVolume called with req: %#v", req)

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: req.CapacityRange.RequiredBytes,
	}, nil
}

func (ns *LinodeNodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	glog.V(4).Infof("NodeGetCapabilities called with req: %#v", req)

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.Driver.nscap,
	}, nil
}

func (ns *LinodeNodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	glog.V(4).Infof("NodeGetInfo called with req: %#v", req)

	top := &csi.Topology{
		Segments: map[string]string{
			"topology.linode.com/region": ns.MetadataService.GetZone(),
		},
	}

	nodeID := ns.MetadataService.GetNodeID()

	resp := &csi.NodeGetInfoResponse{
		NodeId:             strconv.Itoa(nodeID),
		MaxVolumesPerNode:  maxVolumesPerNode,
		AccessibleTopology: top,
	}
	return resp, nil

}

func (ns *LinodeNodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nodeGetVolumeStats(ctx, req)
}
