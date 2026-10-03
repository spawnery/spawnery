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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

func networkMetricsFixture(t *testing.T) (client.Client, *agent.Registry) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "mc"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:          spawneryv1alpha1.ServerGroupEphemeral,
			MaxPlayers:    20,
			PlayableSlots: ptr.To[int32](12),
		},
		Status: spawneryv1alpha1.ServerGroupStatus{Replicas: 2, ReadyReplicas: 1, OnlinePlayers: 5, FreeSlots: 7},
	}
	server := func(name, uid, phase string) *spawneryv1alpha1.Server {
		return &spawneryv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "mc"},
			Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
			Status:     spawneryv1alpha1.ServerStatus{Phase: phase, PodName: name, PodUID: uid},
		}
	}
	pod := func(name, uid, role, group, node string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "mc", UID: types.UID(uid), Labels: map[string]string{
				podspec.LabelRole: role, podspec.LabelGroup: group, podspec.LabelNetwork: "production",
			}},
			Spec: corev1.PodSpec{NodeName: node},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		group,
		server("lobby-a", "u1", "Ready"), server("lobby-b", "u2", "Starting"),
		pod("lobby-a", "u1", podspec.RoleServer, "lobby", "node-1"),
		pod("lobby-b", "u2", podspec.RoleServer, "lobby", "node-2"),
		pod("gateway-x", "p1", podspec.RoleProxy, "gateway", "node-1"),
	).WithStatusSubresource(group).Build()

	now := time.Unix(1000, 0)
	reg := agent.New(func() time.Time { return now }, 5*time.Second, now)
	reg.Connect("u1", agent.RoleServer)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(reg.ReportPlayers("u1", 5, 20))
	must(reg.ReportTicks("u1", 19.5, 12.5))
	must(reg.ReportHeap("u1", 1_000_000_000, 4_000_000_000))
	reg.Connect("p1", agent.RoleProxy)
	must(reg.ReportPlayers("p1", 5, 100))
	must(reg.ReportHeap("p1", 200_000_000, 1_000_000_000))
	return c, reg
}

const networkMetricsExpected = `
# HELP spawnery_network_players Players on the network's proxies.
# TYPE spawnery_network_players gauge
spawnery_network_players{namespace="mc",network="production"} 5
# HELP spawnery_group_servers Servers of the group.
# TYPE spawnery_group_servers gauge
spawnery_group_servers{group="lobby",namespace="mc",network="production",type="Ephemeral"} 2
# HELP spawnery_group_servers_ready Ready servers of the group.
# TYPE spawnery_group_servers_ready gauge
spawnery_group_servers_ready{group="lobby",namespace="mc",network="production",type="Ephemeral"} 1
# HELP spawnery_group_players Players on the group's servers.
# TYPE spawnery_group_players gauge
spawnery_group_players{group="lobby",namespace="mc",network="production",type="Ephemeral"} 5
# HELP spawnery_group_free_slots Free playable seats of the group.
# TYPE spawnery_group_free_slots gauge
spawnery_group_free_slots{group="lobby",namespace="mc",network="production",type="Ephemeral"} 7
# HELP spawnery_server_players Players on the server, as its agent last reported.
# TYPE spawnery_server_players gauge
spawnery_server_players{group="lobby",namespace="mc",network="production",node="node-1",server="lobby-a"} 5
# HELP spawnery_server_slots The server's slots, as its agent last reported.
# TYPE spawnery_server_slots gauge
spawnery_server_slots{group="lobby",namespace="mc",network="production",node="node-1",server="lobby-a"} 20
# HELP spawnery_server_playable_slots The server's effective playable slots.
# TYPE spawnery_server_playable_slots gauge
spawnery_server_playable_slots{group="lobby",namespace="mc",network="production",node="node-1",server="lobby-a"} 12
# HELP spawnery_server_tps The server's one-minute ticks per second.
# TYPE spawnery_server_tps gauge
spawnery_server_tps{group="lobby",namespace="mc",network="production",node="node-1",server="lobby-a"} 19.5
# HELP spawnery_server_mspt The server's mean tick time in milliseconds.
# TYPE spawnery_server_mspt gauge
spawnery_server_mspt{group="lobby",namespace="mc",network="production",node="node-1",server="lobby-a"} 12.5
# HELP spawnery_server_heap_used_bytes JVM heap in use on the server.
# TYPE spawnery_server_heap_used_bytes gauge
spawnery_server_heap_used_bytes{group="lobby",namespace="mc",network="production",node="node-1",server="lobby-a"} 1e+09
# HELP spawnery_server_heap_max_bytes The server's JVM max heap.
# TYPE spawnery_server_heap_max_bytes gauge
spawnery_server_heap_max_bytes{group="lobby",namespace="mc",network="production",node="node-1",server="lobby-a"} 4e+09
# HELP spawnery_server_phase 1 for the server's current phase.
# TYPE spawnery_server_phase gauge
spawnery_server_phase{group="lobby",namespace="mc",network="production",node="node-1",phase="Ready",server="lobby-a"} 1
spawnery_server_phase{group="lobby",namespace="mc",network="production",node="node-2",phase="Starting",server="lobby-b"} 1
# HELP spawnery_proxy_players Players on the proxy.
# TYPE spawnery_proxy_players gauge
spawnery_proxy_players{group="gateway",namespace="mc",network="production",node="node-1",proxy="gateway-x"} 5
# HELP spawnery_proxy_heap_used_bytes JVM heap in use on the proxy.
# TYPE spawnery_proxy_heap_used_bytes gauge
spawnery_proxy_heap_used_bytes{group="gateway",namespace="mc",network="production",node="node-1",proxy="gateway-x"} 2e+08
# HELP spawnery_proxy_heap_max_bytes The proxy's JVM max heap.
# TYPE spawnery_proxy_heap_max_bytes gauge
spawnery_proxy_heap_max_bytes{group="gateway",namespace="mc",network="production",node="node-1",proxy="gateway-x"} 1e+09
`

// A server that has not reported yet has its phase and nothing else: a zero
// TPS would drag every "worst TPS" panel to the floor.
func TestNetworkCollectorEmitsWhatTheNetworkIs(t *testing.T) {
	c, reg := networkMetricsFixture(t)
	col := &NetworkCollector{}
	col.Bind(c, reg)

	if err := testutil.CollectAndCompare(col, strings.NewReader(networkMetricsExpected)); err != nil {
		t.Fatal(err)
	}
}

// Computed at scrape time: a server that is gone leaves no series behind.
func TestNetworkCollectorForgetsADeletedServer(t *testing.T) {
	c, reg := networkMetricsFixture(t)
	col := &NetworkCollector{}
	col.Bind(c, reg)
	if n := testutil.CollectAndCount(col, "spawnery_server_phase"); n != 2 {
		t.Fatalf("phase series = %d before the delete, want 2", n)
	}
	gone := &spawneryv1alpha1.Server{ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "mc"}}
	if err := c.Delete(context.Background(), gone); err != nil {
		t.Fatal(err)
	}

	if n := testutil.CollectAndCount(col, "spawnery_server_phase"); n != 1 {
		t.Fatalf("phase series = %d after the delete, want 1", n)
	}
	if n := testutil.CollectAndCount(col, "spawnery_server_tps"); n != 0 {
		t.Fatalf("tps series = %d after the delete, want 0", n)
	}
}

// Registered at init for the metrics reference, bound later: until then it
// emits nothing rather than failing a scrape.
func TestAnUnboundNetworkCollectorEmitsNothing(t *testing.T) {
	if n := testutil.CollectAndCount(&NetworkCollector{}); n != 0 {
		t.Fatalf("an unbound collector emitted %d series", n)
	}
}

// A pod whose agent is gone keeps its registry entry; its last TPS and heap
// would otherwise stand as a flat line exactly while it is broken.
func TestNetworkCollectorDropsTheFiguresOfADisconnectedAgent(t *testing.T) {
	c, reg := networkMetricsFixture(t)
	reg.Disconnect("u1")
	reg.Disconnect("p1")
	col := &NetworkCollector{}
	col.Bind(c, reg)

	for _, name := range []string{"spawnery_server_players", "spawnery_server_tps", "spawnery_server_heap_used_bytes",
		"spawnery_proxy_players", "spawnery_proxy_heap_used_bytes", "spawnery_network_players"} {
		if n := testutil.CollectAndCount(col, name); n != 0 {
			t.Errorf("%s: %d series for a disconnected agent, want 0", name, n)
		}
	}
	if n := testutil.CollectAndCount(col, "spawnery_server_phase"); n != 2 {
		t.Errorf("phase series = %d, want both servers still", n)
	}
}
