/*
Copyright paul_wtf.

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

package worldsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/spawnery/spawnery/internal/prune"
)

const (
	DriverName      = "worldsync.spawnery.cloud"
	ctxEphemeral    = "csi.storage.k8s.io/ephemeral"
	ctxPodNamespace = "csi.storage.k8s.io/pod.namespace"
	ctxPodName      = "csi.storage.k8s.io/pod.name"
	AttrWorld       = "world"
	AttrKeep        = "keep"
)

type CSIServer struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer
	node    *Node
	nodeID  string
	version string
}

func NewCSIServer(node *Node, nodeID, version string) *CSIServer {
	return &CSIServer{node: node, nodeID: nodeID, version: version}
}

func (s *CSIServer) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: s.version}, nil
}

func (s *CSIServer) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (s *CSIServer) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func (s *CSIServer) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: s.nodeID}, nil
}

func (s *CSIServer) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (s *CSIServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume id and a target path are required")
	}
	vc := req.GetVolumeContext()
	if vc[ctxEphemeral] != "true" {
		return nil, status.Error(codes.InvalidArgument, "only inline ephemeral volumes are served")
	}
	world, ns := vc[AttrWorld], vc[ctxPodNamespace]
	if err := checkWorld(world); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if strings.Split(world, "/")[0] != ns {
		return nil, status.Errorf(codes.PermissionDenied, "a pod in namespace %q asked for world %q", ns, world)
	}
	if vc[AttrKeep] == "" {
		return nil, status.Error(codes.InvalidArgument, "the keep attribute is empty")
	}
	keep := strings.Split(vc[AttrKeep], "\n")
	if _, err := prune.ParseKeep(keep); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	err := s.node.Publish(ctx, PublishRequest{World: world, Keep: keep, Target: req.GetTargetPath(), Pod: ns + "/" + vc[ctxPodName]})
	switch {
	case errors.Is(err, ErrUnavailable):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *CSIServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume id and a target path are required")
	}
	if err := s.node.Unpublish(ctx, req.GetTargetPath()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = os.Remove(filepath.Clean(req.GetTargetPath()))
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// Import uploads dir's kept paths as generation 1 of a world that has none.
func Import(ctx context.Context, st Store, base, world string, keep []string, dir string) (Manifest, error) {
	if err := checkWorld(world); err != nil {
		return Manifest{}, err
	}
	k, err := prune.ParseKeep(keep)
	if err != nil {
		return Manifest{}, err
	}
	snap, err := os.MkdirTemp("", "worldsync-import-")
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = os.RemoveAll(snap) }()
	if _, err := TakeSnapshot(dir, snap, k, nil, 1); err != nil {
		return Manifest{}, err
	}
	m, _, err := UploadSnapshot(ctx, st, WorldPrefix(base, world), snap, nil, "", NewWorldID)
	return m, err
}
