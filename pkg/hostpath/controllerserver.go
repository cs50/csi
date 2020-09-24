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
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	deviceID           = "deviceID"
	maxStorageCapacity = tib
)

type accessType int

const mountAccess accessType = iota

type controllerServer struct {
	caps   []*csi.ControllerServiceCapability
	nodeID string
}

func NewControllerServer(nodeID string) *controllerServer {
	return &controllerServer{
		caps: getControllerServiceCapabilities(
			[]csi.ControllerServiceCapability_RPC_Type{
				csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
				// TODO (kzidane) add LIST_VOLUMES capability
				csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
			}),
		nodeID: nodeID,
	}
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := cs.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}

	caps := req.GetVolumeCapabilities()
	if caps == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	// Keep a record of the requested access types.
	var accessTypeMount bool

	for _, cap := range caps {
		if cap.GetMount() != nil {
			accessTypeMount = true
			break
		}
	}

	// A real driver would also need to check that the other
	// fields in VolumeCapabilities are sane. The check above is
	// just enough to pass the "[Testpattern: Dynamic PV (block
	// volmode)] volumeMode should fail in binding dynamic
	// provisioned PV to PVC" storage E2E test.

	if !accessTypeMount {
		return nil, status.Error(codes.InvalidArgument, "access type not supported")
	}

	// Check for maximum available capacity
	capacity := int64(req.GetCapacityRange().GetRequiredBytes())
	if capacity >= maxStorageCapacity {
		return nil, status.Errorf(codes.OutOfRange, "Requested capacity %d exceeds maximum allowed %d", capacity, maxStorageCapacity)
	}

	// Need to check for already existing volume name, and if found
	// check for the requested capacity and already allocated capacity
	if exVol, err := getVolumeByID(req.GetName()); err == nil {
		// Since err is nil, it means the volume with the same name already exists
		// need to check if the size of existing volume is the same as in new
		// request
		if exVol.VolSize < capacity {
			return nil, status.Errorf(codes.AlreadyExists, "Volume with the same name: %s but with different size already exist", req.GetName())
		}

		// TODO (sbezverk) Do I need to make sure that volume still exists?
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      exVol.VolID,
				CapacityBytes: int64(exVol.VolSize),
				VolumeContext: req.GetParameters(),
				ContentSource: req.GetVolumeContentSource(),
			},
		}, nil
	}

	volumeID := req.GetName()
	vol, err := createHostpathVolume(volumeID, capacity, mountAccess)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create volume %v: %v", volumeID, err)
	}

	glog.V(4).Infof("created volume %s at path %s", vol.VolID, vol.VolPath)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext: req.GetParameters(),
			ContentSource: req.GetVolumeContentSource(),
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid delete volume req: %v", req)
		return nil, err
	}

	volId := req.GetVolumeId()
	if err := deleteHostpathVolume(volId); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete volume %v: %v", volId, err)
	}

	glog.V(4).Infof("volume %v successfully deleted", volId)

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.caps,
	}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, req.VolumeId)
	}

	if _, err := getVolumeByID(req.GetVolumeId()); err != nil {
		return nil, status.Error(codes.NotFound, req.GetVolumeId())
	}

	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetMount() == nil {
			return nil, status.Error(codes.InvalidArgument, "access type not supported")
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// TODO (kzidane) implement ListVolumes
func (cs *controllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {

	volID := req.GetVolumeId()
	if len(volID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	capRange := req.GetCapacityRange()
	if capRange == nil {
		return nil, status.Error(codes.InvalidArgument, "Capacity range not provided")
	}

	capacity := int64(capRange.GetRequiredBytes())
	if capacity >= maxStorageCapacity {
		return nil, status.Errorf(codes.OutOfRange, "Requested capacity %d exceeds maximum allowed %d", capacity, maxStorageCapacity)
	}

	exVol, err := getVolumeByID(volID)
	if err != nil {
		// Assume not found error
		return nil, status.Errorf(codes.NotFound, "Could not get volume %s: %v", volID, err)
	}

	if exVol.VolSize < capacity {

		if err := expandVolume(volID, capacity); err != nil {
			return nil, status.Errorf(codes.Internal, "Could not expand volume %s: %v", volID, err)
		}

		exVol.VolSize = capacity
		if err := updateHostpathVolume(volID, exVol); err != nil {
			return nil, status.Errorf(codes.Internal, "Could not update volume %s: %v", volID, err)
		}
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         exVol.VolSize,
		NodeExpansionRequired: true,
	}, nil
}

func (cs *controllerServer) validateControllerServiceRequest(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}

	for _, cap := range cs.caps {
		if c == cap.GetRpc().GetType() {
			return nil
		}
	}
	return status.Errorf(codes.InvalidArgument, "unsupported capability %s", c)
}

func getControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) []*csi.ControllerServiceCapability {
	var csc []*csi.ControllerServiceCapability

	for _, cap := range cl {
		glog.Infof("Enabling controller service capability: %v", cap.String())
		csc = append(csc, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		})
	}

	return csc
}
