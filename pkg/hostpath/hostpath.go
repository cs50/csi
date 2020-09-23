/*
Copyright 2017 The Kubernetes Authors.

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

package hostpath

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	utilexec "k8s.io/utils/exec"

	timestamp "github.com/golang/protobuf/ptypes/timestamp"

	lru "github.com/hashicorp/golang-lru"
)

const (
	kib    int64 = 1024
	mib    int64 = kib * 1024
	gib    int64 = mib * 1024
	gib100 int64 = gib * 100
	tib    int64 = gib * 1024
	tib100 int64 = tib * 100
)

type hostPath struct {
	name              string
	nodeID            string
	version           string
	endpoint          string
	maxVolumesPerNode int64

	ids *identityServer
	ns  *nodeServer
	cs  *controllerServer
}

type hostPathVolume struct {
	VolName       string     `json:"volName"`
	VolID         string     `json:"volID"`
	VolSize       int64      `json:"volSize"`
	VolPath       string     `json:"volPath"`
	VolAccessType accessType `json:"volAccessType"`
	ParentVolID   string     `json:"parentVolID,omitempty"`
	ParentSnapID  string     `json:"parentSnapID,omitempty"`
}

type hostPathSnapshot struct {
	Name         string              `json:"name"`
	Id           string              `json:"id"`
	VolID        string              `json:"volID"`
	Path         string              `json:"path"`
	CreationTime timestamp.Timestamp `json:"creationTime"`
	SizeBytes    int64               `json:"sizeBytes"`
	ReadyToUse   bool                `json:"readyToUse"`
}

var (
	vendorVersion = "dev"

	// TODO (kzidane) estimate max size of volumes
	hostPathVolumes *lru.Cache

	hostPathVolumeSnapshots map[string]hostPathSnapshot
)

const (
	// Directory where data for volumes and snapshots are persisted.
	dataRoot = "/csi-data-dir"

	// Extension with which snapshot files will be saved.
	snapshotExt = ".snap"
)

func init() {
	// hostPathVolumes = map[string]hostPathVolume{}
	var err error
	hostPathVolumes, err = lru.New(128)
	if err != nil {
		glog.Errorf("failed to initialize hostPathVolumes LRU cach: %v", err)
	}

	hostPathVolumeSnapshots = map[string]hostPathSnapshot{}
}

func NewHostPathDriver(driverName, nodeID, endpoint string, maxVolumesPerNode int64, version string) (*hostPath, error) {
	if driverName == "" {
		return nil, errors.New("no driver name provided")
	}

	if nodeID == "" {
		return nil, errors.New("no node id provided")
	}

	if endpoint == "" {
		return nil, errors.New("no driver endpoint provided")
	}
	if version != "" {
		vendorVersion = version
	}

	if err := os.MkdirAll(dataRoot, 0750); err != nil {
		return nil, fmt.Errorf("failed to create dataRoot: %v", err)
	}

	glog.Infof("Driver: %v ", driverName)
	glog.Infof("Version: %s", vendorVersion)

	return &hostPath{
		name:              driverName,
		version:           vendorVersion,
		nodeID:            nodeID,
		endpoint:          endpoint,
		maxVolumesPerNode: maxVolumesPerNode,
	}, nil
}

func getSnapshotID(file string) (bool, string) {
	glog.V(4).Infof("file: %s", file)
	// Files with .snap extension are volumesnapshot files.
	// e.g. foo.snap, foo.bar.snap
	if filepath.Ext(file) == snapshotExt {
		return true, strings.TrimSuffix(file, snapshotExt)
	}
	return false, ""
}

func discoverExistingSnapshots() {
	glog.V(4).Infof("discovering existing snapshots in %s", dataRoot)
	files, err := ioutil.ReadDir(dataRoot)
	if err != nil {
		glog.Errorf("failed to discover snapshots under %s: %v", dataRoot, err)
	}
	for _, file := range files {
		isSnapshot, snapshotID := getSnapshotID(file.Name())
		if isSnapshot {
			glog.V(4).Infof("adding snapshot %s from file %s", snapshotID, getSnapshotPath(snapshotID))
			hostPathVolumeSnapshots[snapshotID] = hostPathSnapshot{
				Id:         snapshotID,
				Path:       getSnapshotPath(snapshotID),
				ReadyToUse: true,
			}
		}
	}
}

func (hp *hostPath) Run() {
	// Create GRPC servers
	hp.ids = NewIdentityServer(hp.name, hp.version)
	hp.ns = NewNodeServer(hp.nodeID, hp.maxVolumesPerNode)
	hp.cs = NewControllerServer(hp.nodeID)

	discoverExistingSnapshots()
	s := NewNonBlockingGRPCServer()
	s.Start(hp.endpoint, hp.ids, hp.cs, hp.ns)
	s.Wait()
}

func getVolumeByID(volumeID string) (hostPathVolume, error) {
	hostPathVol, ok := hostPathVolumes.Get(volumeID)
	if ok {
		return hostPathVol.(hostPathVolume), nil
	}

	// TODO (kzidane) load volume from Dynamodb table

	return hostPathVolume{}, fmt.Errorf("volume id %s does not exist in the volumes list", volumeID)
}

func getSnapshotByName(name string) (hostPathSnapshot, error) {
	for _, snapshot := range hostPathVolumeSnapshots {
		if snapshot.Name == name {
			return snapshot, nil
		}
	}
	return hostPathSnapshot{}, fmt.Errorf("snapshot name %s does not exist in the snapshots list", name)
}

// getVolumePath returns the canonical path for hostpath volume
func getVolumePath(volID string) string {
	return filepath.Join(dataRoot, volID)
}

// createVolume create the directory for the hostpath volume.
// It returns the volume path or err if one occurs.
func createHostpathVolume(volID string, cap int64, volAccessType accessType) (*hostPathVolume, error) {
	path := getVolumePath(volID)
	if volAccessType == mountAccess {
		executor := utilexec.New()
		size := fmt.Sprintf("%dM", cap/mib)

		// Create a block file.
		out, err := executor.Command("truncate", "-s", size, path).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("failed to create block device: %v, %v", err, string(out))
		}

		out, err = executor.Command("mkfs.ext4", path).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("failed to create file system on device: %v, %v", err, string(out))
		}
	} else {
		return nil, fmt.Errorf("unsupported access type %v", volAccessType)
	}

	hostpathVol := hostPathVolume{
		VolID:         volID,
		VolSize:       cap,
		VolPath:       path,
		VolAccessType: volAccessType,
	}

	// TODO (kzidane) Write to Dynamodb table

	hostPathVolumes.Add(volID, hostpathVol)
	return &hostpathVol, nil
}

func expandVolume(volID string, cap int64) error {
	path := getVolumePath(volID)
	executor := utilexec.New()
	size := fmt.Sprintf("%dM", cap/mib)

	out, err := executor.Command("truncate", "-s", size, path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to expand volume: %v, %v", err, string(out))
	}

	return nil
}

// updateVolume updates the existing hostpath volume.
func updateHostpathVolume(volID string, volume hostPathVolume) error {
	glog.V(4).Infof("updating hostpath volume: %s", volID)

	if _, err := getVolumeByID(volID); err != nil {
		return err
	}

	hostPathVolumes.Add(volID, volume)
	return nil
}

// deleteVolume deletes the directory for the hostpath volume.
func deleteHostpathVolume(volID string) error {
	glog.V(4).Infof("deleting hostpath volume: %s", volID)

	path := getVolumePath(volID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}

	// delete(hostPathVolumes, volID)
	hostPathVolumes.Remove(volID)
	return nil
}

// hostPathIsEmpty is a simple check to determine if the specified hostpath directory
// is empty or not.
func hostPathIsEmpty(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return true, fmt.Errorf("unable to open hostpath volume, error: %v", err)
	}
	defer f.Close()

	_, err = f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

// loadFromSnapshot populates the given destPath with data from the snapshotID
func loadFromSnapshot(size int64, snapshotId, destPath string, mode accessType) error {
	snapshot, ok := hostPathVolumeSnapshots[snapshotId]
	if !ok {
		return status.Errorf(codes.NotFound, "cannot find snapshot %v", snapshotId)
	}
	if snapshot.ReadyToUse != true {
		return status.Errorf(codes.Internal, "snapshot %v is not yet ready to use.", snapshotId)
	}
	if snapshot.SizeBytes > size {
		return status.Errorf(codes.InvalidArgument, "snapshot %v size %v is greater than requested volume size %v", snapshotId, snapshot.SizeBytes, size)
	}
	snapshotPath := snapshot.Path

	var cmd []string
	if mode == mountAccess {
		cmd = []string{"dd", "if=" + snapshotPath, "of=" + destPath}
	} else {
		return status.Errorf(codes.InvalidArgument, "unknown accessType: %d", mode)
	}

	executor := utilexec.New()
	out, err := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return status.Errorf(codes.Internal, "failed pre-populate data from snapshot %v: %v: %s", snapshotId, err, out)
	}
	return nil
}
