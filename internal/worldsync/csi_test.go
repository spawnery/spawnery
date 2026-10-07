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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func csiFor(t *testing.T) (*CSIServer, *harness) {
	h := newHarness(t)
	return NewCSIServer(h.node("a"), "a", "test"), h
}

func publishReq(t *testing.T, ns, world string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:   "csi-123",
		TargetPath: filepath.Join(t.TempDir(), "mount"),
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/ephemeral":     "true",
			"csi.storage.k8s.io/pod.namespace": ns,
			"csi.storage.k8s.io/pod.name":      "g-k",
			"world":                            world,
			"keep":                             "worlds/world",
		},
	}
}

func TestAPodCannotMountAWorldOfAnotherNamespace(t *testing.T) {
	s, _ := csiFor(t)
	_, err := s.NodePublishVolume(context.Background(), publishReq(t, "attacker", "victim/g/k"))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestOnlyEphemeralVolumesArePublished(t *testing.T) {
	s, _ := csiFor(t)
	req := publishReq(t, "ns", "ns/g/k")
	delete(req.VolumeContext, "csi.storage.k8s.io/ephemeral")
	if _, err := s.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAHeldWorldIsUnavailableOverCSI(t *testing.T) {
	s, h := csiFor(t)
	if _, err := h.publish("b", "ns/g/k", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	_, err := s.NodePublishVolume(context.Background(), publishReq(t, "ns", "ns/g/k"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable so the kubelet retries", status.Code(err))
	}
}

func TestUnpublishOfAnUnknownTargetSucceeds(t *testing.T) {
	s, _ := csiFor(t)
	if _, err := s.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "x", TargetPath: filepath.Join(t.TempDir(), "gone")}); err != nil {
		t.Fatalf("idempotent unpublish: %v", err)
	}
}

func TestPluginInfoNamesTheDriver(t *testing.T) {
	s, _ := csiFor(t)
	info, err := s.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil || info.GetName() != DriverName {
		t.Fatalf("info = %v, %v", info, err)
	}
}

func TestTheNodeAdvertisesTheMountGroup(t *testing.T) {
	s, _ := csiFor(t)
	caps, err := s.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP {
			return
		}
	}
	t.Fatalf("capabilities %v lack VOLUME_MOUNT_GROUP; the kubelet passes no fsGroup without it", caps.GetCapabilities())
}

func TestImportRefusesAnExistingWorld(t *testing.T) {
	st := NewMemStore(time.Now)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	if _, err := Import(context.Background(), st, "", "ns/g/k", []string{"worlds/world"}, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), st, "", "ns/g/k", []string{"worlds/world"}, dir); err == nil {
		t.Fatal("a second import over an existing world succeeded")
	}
}

func TestAMalformedWorldIsInvalidArgument(t *testing.T) {
	for _, world := range []string{"attacker/../victim", "ns/g/k/x", "/ns/g/k", "ns//k", "ns/g/.."} {
		t.Run(world, func(t *testing.T) {
			s, _ := csiFor(t)
			_, err := s.NodePublishVolume(context.Background(), publishReq(t, strings.Split(world, "/")[0], world))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v (%v), want InvalidArgument", status.Code(err), err)
			}
		})
	}
}

func TestAMalformedKeepIsInvalidArgument(t *testing.T) {
	s, _ := csiFor(t)
	req := publishReq(t, "ns", "ns/g/k")
	req.VolumeContext["keep"] = "worlds/world\n../escape"
	if _, err := s.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (%v), want InvalidArgument", status.Code(err), err)
	}
}

func TestAnEmptyVolumeIDOrTargetIsInvalidArgument(t *testing.T) {
	s, _ := csiFor(t)
	for name, mod := range map[string]func(*csi.NodePublishVolumeRequest){
		"volume id": func(r *csi.NodePublishVolumeRequest) { r.VolumeId = "" },
		"target":    func(r *csi.NodePublishVolumeRequest) { r.TargetPath = "" },
	} {
		req := publishReq(t, "ns", "ns/g/k")
		mod(req)
		if _, err := s.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("publish without %s: code = %v, want InvalidArgument", name, status.Code(err))
		}
	}
	for name, req := range map[string]*csi.NodeUnpublishVolumeRequest{
		"volume id": {TargetPath: filepath.Join(t.TempDir(), "gone")},
		"target":    {VolumeId: "x"},
	} {
		if _, err := s.NodeUnpublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("unpublish without %s: code = %v, want InvalidArgument", name, status.Code(err))
		}
	}
}

func TestImportRefusesAMalformedWorld(t *testing.T) {
	st := NewMemStore(time.Now)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	if _, err := Import(context.Background(), st, "", "ns/../victim", []string{"worlds/world"}, dir); err == nil {
		t.Fatal("an import into ns/../victim succeeded")
	}
	if keys := st.Keys(); len(keys) != 0 {
		t.Fatalf("the refused import wrote %v", keys)
	}
}
