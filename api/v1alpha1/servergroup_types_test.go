package v1alpha1

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"
)

func TestFinishedRetentionIsSecondsNotTheFailedOne(t *testing.T) {
	g := &ServerGroup{}
	g.Spec.FailedRetentionSeconds = 3600
	g.Spec.FinishedRetentionSeconds = 300

	if got, want := g.FinishedRetention(), 5*time.Minute; got != want {
		t.Errorf("FinishedRetention() = %v, want %v", got, want)
	}
	if got, want := g.FailedRetention(), time.Hour; got != want {
		t.Errorf("FailedRetention() = %v, want %v", got, want)
	}
}

func TestDesiredReplicasIsZeroForOnDemand(t *testing.T) {
	g := &ServerGroup{
		Spec: ServerGroupSpec{Type: ServerGroupOnDemand, Replicas: ptr.To[int32](3)},
	}
	if got := g.DesiredReplicas(); got != 0 {
		t.Fatalf("DesiredReplicas() = %d, want 0", got)
	}
	if !g.IsOnDemand() {
		t.Fatal("IsOnDemand() = false for a group of type OnDemand")
	}
	if g.IsEphemeral() {
		t.Fatal("IsEphemeral() = true for a group of type OnDemand")
	}
}

func TestUpdateStrategyAndFloorAccessors(t *testing.T) {
	cases := []struct {
		name      string
		update    *UpdateSpec
		whenEmpty bool
		floor     int32
	}{
		{"no update policy", nil, false, 0},
		{"rolling update without a floor", &UpdateSpec{Strategy: UpdateRollingUpdate}, false, 0},
		{"when empty", &UpdateSpec{Strategy: UpdateWhenEmpty}, true, 0},
		{"a floor", &UpdateSpec{MinAvailable: ptr.To[int32](3)}, false, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &ServerGroup{Spec: ServerGroupSpec{Update: tc.update}}
			if got := g.UpdateWhenEmpty(); got != tc.whenEmpty {
				t.Errorf("UpdateWhenEmpty() = %v, want %v", got, tc.whenEmpty)
			}
			if got := g.UpdateMinAvailable(); got != tc.floor {
				t.Errorf("UpdateMinAvailable() = %d, want %d", got, tc.floor)
			}
		})
	}
}

func TestJoinPermissionResolvesItsNode(t *testing.T) {
	var none *JoinPermission
	if got := none.ResolvedNode("vip"); got != "" {
		t.Errorf("no rule resolved to %q, want empty", got)
	}
	if none.DenyOnly() {
		t.Error("no rule reads as deny-only")
	}
	if got := (&JoinPermission{}).ResolvedNode("vip"); got != "spawnery.join.vip" {
		t.Errorf("default node = %q, want spawnery.join.vip", got)
	}
	named := &JoinPermission{Node: "network.vip", Mode: JoinPermissionDenyOnly}
	if got := named.ResolvedNode("vip"); got != "network.vip" {
		t.Errorf("named node = %q, want network.vip", got)
	}
	if !named.DenyOnly() {
		t.Error("DenyOnly mode does not read as deny-only")
	}
	if (&JoinPermission{Mode: JoinPermissionRequired}).DenyOnly() {
		t.Error("Required mode reads as deny-only")
	}
}

func TestUsesClaim(t *testing.T) {
	g := &ServerGroup{Spec: ServerGroupSpec{Type: ServerGroupOnDemand, Storage: &StorageSpec{}}}
	if !g.UsesClaim() || g.UsesObjectStore() {
		t.Fatal("an OnDemand group without backend uses a claim")
	}
	g.Spec.Storage.Backend = StorageBackendObjectStore
	if g.UsesClaim() || !g.UsesObjectStore() {
		t.Fatal("ObjectStore uses no claim")
	}
	e := &ServerGroup{Spec: ServerGroupSpec{Type: ServerGroupEphemeral}}
	if e.UsesClaim() {
		t.Fatal("an Ephemeral group uses no claim")
	}
}

func TestWorldRetentionIsTheObjectStoresOnly(t *testing.T) {
	r := &RetentionSpec{Last: 12, Daily: 7}
	g := &ServerGroup{Spec: ServerGroupSpec{Type: ServerGroupOnDemand, Storage: &StorageSpec{Keep: []string{"world"}, Retention: r}}}
	if got := g.WorldRetention(); got != (RetentionSpec{}) {
		t.Fatalf("a group on claims has world retention %+v", got)
	}
	g.Spec.Storage.Backend = StorageBackendObjectStore
	if got := g.WorldRetention(); got != *r {
		t.Fatalf("WorldRetention = %+v, want %+v", got, *r)
	}
	g.Spec.Storage.Retention = nil
	if got := g.WorldRetention(); got != (RetentionSpec{}) {
		t.Fatalf("WorldRetention without retention = %+v, want zero", got)
	}
}
