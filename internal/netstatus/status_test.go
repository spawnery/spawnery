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

package netstatus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/podspec"
)

const ns = "games"

var t0 = time.Unix(10_000, 0)

type fixedMetrics struct {
	usage map[string]Usage
	err   error
}

func (f fixedMetrics) PodUsage(context.Context, string) (map[string]Usage, error) {
	return f.usage, f.err
}

func pod(name, role, group, cpuReq, cpuLim, memReq, memLim string, uid string) *corev1.Pod {
	res := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	if cpuReq != "" {
		res.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if cpuLim != "" {
		res.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
	}
	if memReq != "" {
		res.Requests[corev1.ResourceMemory] = resource.MustParse(memReq)
	}
	if memLim != "" {
		res.Limits[corev1.ResourceMemory] = resource.MustParse(memLim)
	}
	labels := map[string]string{}
	if role != "" {
		labels[podspec.LabelRole] = role
		labels[podspec.LabelGroup] = group
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, UID: k8stypes.UID(uid),
			CreationTimestamp: metav1.NewTime(t0.Add(-90 * time.Minute))},
		Spec:   corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "main", Resources: res}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// network: server group lobby (lobby-a, lobby-b), on-demand group rooms with
// the private server rooms-x, proxy group gateway (gateway-a), and one pod
// that belongs to no group (db-0).
func network(t *testing.T) (client.Client, *agent.Registry) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lobby := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns}}
	lobby.Spec.Type = spawneryv1alpha1.ServerGroupEphemeral
	lobby.Status.Phase = "Ready"
	lobby.Status.Replicas, lobby.Status.ReadyReplicas, lobby.Status.OnlinePlayers = 2, 2, 5
	rooms := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "rooms", Namespace: ns}}
	rooms.Spec.Type = spawneryv1alpha1.ServerGroupOnDemand
	gateway := &spawneryv1alpha1.ProxyGroup{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns}}
	gateway.Spec.Replicas = 1
	gateway.Status.Phase, gateway.Status.ReadyReplicas, gateway.Status.ConnectedPlayers = "Ready", 1, 6

	server := func(name, group, podUID, ph string) *spawneryv1alpha1.Server {
		s := &spawneryv1alpha1.Server{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			CreationTimestamp: metav1.NewTime(t0.Add(-2 * time.Hour))}}
		s.Spec.GroupRef.Name = group
		s.Status.Phase, s.Status.PodName, s.Status.PodUID = ph, name, podUID
		s.Status.Players, s.Status.Slots = 2, 20
		return s
	}
	lobbyA := server("lobby-a", "lobby", "uid-la", "Ready")
	lobbyB := server("lobby-b", "lobby", "uid-lb", "Retiring")
	lobbyB.Spec.Retire = true
	lobbyB.Status.PlayableSlots = 12
	roomsX := server("rooms-x", "rooms", "uid-rx", "Ready")
	roomsX.Spec.Key = "somebody"

	gw := pod("gateway-a", podspec.RoleProxy, "gateway", "100m", "500m", "256Mi", "512Mi", "uid-ga")
	gw.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	gw.Spec.NodeName = "node-3"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		lobby, rooms, gateway, lobbyA, lobbyB, roomsX,
		pod("lobby-a", podspec.RoleServer, "lobby", "500m", "1", "1Gi", "2Gi", "uid-la"),
		pod("lobby-b", podspec.RoleServer, "lobby", "500m", "1", "1Gi", "2Gi", "uid-lb"),
		pod("rooms-x", podspec.RoleServer, "rooms", "250m", "", "512Mi", "1Gi", "uid-rx"),
		gw,
		pod("db-0", "", "", "200m", "", "256Mi", "", "uid-db"),
	).Build()

	reg := agent.New(func() time.Time { return t0 }, 5*time.Second)
	for _, key := range []string{"uid-la", "uid-lb", "uid-rx", "uid-ga"} {
		reg.Connect(key, agent.RoleServer)
	}
	report := func(key string, players, slots int32, tps, mspt float64) {
		if err := reg.ReportPlayers(key, players, slots); err != nil {
			t.Fatal(err)
		}
		if err := reg.ReportTicks(key, tps, mspt); err != nil {
			t.Fatal(err)
		}
	}
	report("uid-la", 2, 20, 19.9, 8)
	report("uid-lb", 3, 20, 16.5, 42)
	report("uid-rx", 1, 10, 20, 3)
	report("uid-ga", 6, 500, 0, 0)
	return c, reg
}

func allMeasured() fixedMetrics {
	return fixedMetrics{usage: map[string]Usage{
		"lobby-a":   {CPUMilli: 400, MemoryBytes: 1 << 30},
		"lobby-b":   {CPUMilli: 700, MemoryBytes: 3 << 29},
		"rooms-x":   {CPUMilli: 100, MemoryBytes: 1 << 28},
		"gateway-a": {CPUMilli: 50, MemoryBytes: 1 << 28},
		"db-0":      {CPUMilli: 20, MemoryBytes: 1 << 27},
	}}
}

func status(t *testing.T, m MetricsReader, audience netstate.Audience, target string) (*agentpb.StatusResult, error) {
	t.Helper()
	c, reg := network(t)
	return Source{Reader: c, Agents: reg, Metrics: m, Clock: func() time.Time { return t0 }}.
		Status(context.Background(), ns, audience, target)
}

func groupNamed(res *agentpb.StatusResult, name string) *agentpb.GroupStatus {
	for _, g := range res.GetGroups() {
		if g.GetName() == name {
			return g
		}
	}
	return nil
}

func TestStatusNetworkForProxies(t *testing.T) {
	res, err := status(t, allMeasured(), netstate.ForProxies, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.GetMetricsAvailable() {
		t.Fatal("metrics_available false with a working metrics API")
	}
	tot := res.GetTotal()
	if tot.GetPods() != 5 || tot.GetPodsMeasured() != 5 {
		t.Errorf("total pods %d/%d, want 5/5", tot.GetPodsMeasured(), tot.GetPods())
	}
	if tot.GetCpuUsedMillicores() != 1270 || tot.GetCpuRequestedMillicores() != 1550 {
		t.Errorf("total cpu used/requested %d/%d, want 1270/1550",
			tot.GetCpuUsedMillicores(), tot.GetCpuRequestedMillicores())
	}
	if !tot.GetCpuUnlimited() || !tot.GetMemoryUnlimited() {
		t.Error("rooms-x and db-0 have no CPU limit and db-0 no memory limit, yet the total claims limits")
	}
	if len(res.GetGroups()) != 3 {
		t.Fatalf("groups = %v, want lobby, rooms and gateway", res.GetGroups())
	}
	lobby := groupNamed(res, "lobby")
	if lobby.GetLowestTps() != 16.5 || lobby.GetUsage().GetPods() != 2 || lobby.GetPlayers() != 5 {
		t.Errorf("lobby = %+v, want lowest TPS 16.5, 2 pods, 5 players", lobby)
	}
	gw := groupNamed(res, "gateway")
	if gw.GetKind() != agentpb.GroupState_PROXY || gw.GetLowestTps() != 0 {
		t.Errorf("gateway = %+v, want a proxy group without TPS", gw)
	}
	if o := res.GetOther(); o.GetPods() != 1 || o.GetCpuUsedMillicores() != 20 {
		t.Errorf("other = %+v, want db-0 alone", o)
	}
	if res.GetPlayers() != 6 || res.GetServers() != 3 || res.GetProxies() != 1 {
		t.Errorf("header %d players, %d servers, %d proxies; want 6, 3, 1",
			res.GetPlayers(), res.GetServers(), res.GetProxies())
	}
	if len(res.GetInstances()) != 0 {
		t.Error("the network form listed instances")
	}
}

func TestStatusHidesPrivateFromServers(t *testing.T) {
	res, err := status(t, allMeasured(), netstate.ForServers, "")
	if err != nil {
		t.Fatal(err)
	}
	if groupNamed(res, "rooms") != nil {
		t.Error("a server agent's answer lists the on-demand group")
	}
	if res.GetServers() != 2 {
		t.Errorf("servers = %d, want 2 without the private one", res.GetServers())
	}
	if res.GetTotal().GetPods() != 5 {
		t.Errorf("total pods = %d; hiding a name must not hide its usage from the total", res.GetTotal().GetPods())
	}
	if res.GetOther().GetPods() != 2 {
		t.Errorf("other pods = %d, want db-0 and rooms-x", res.GetOther().GetPods())
	}
	for _, target := range []string{"rooms", "rooms-x"} {
		if _, err := status(t, allMeasured(), netstate.ForServers, target); !errors.Is(err, ErrUnknownTarget) {
			t.Errorf("target %q from a server agent: err = %v, want ErrUnknownTarget", target, err)
		}
	}
	if _, err := status(t, allMeasured(), netstate.ForProxies, "rooms-x"); err != nil {
		t.Errorf("a proxy may ask about a private server: %v", err)
	}
}

func TestStatusGroupTarget(t *testing.T) {
	res, err := status(t, allMeasured(), netstate.ForProxies, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetGroups()) != 1 || res.GetGroups()[0].GetName() != "lobby" {
		t.Fatalf("groups = %v, want lobby alone", res.GetGroups())
	}
	if len(res.GetInstances()) != 2 || res.GetInstances()[0].GetName() != "lobby-a" {
		t.Fatalf("instances = %v, want lobby-a and lobby-b in order", res.GetInstances())
	}
	b := res.GetInstances()[1]
	if !b.GetRetiring() || b.GetTps() != 16.5 || b.GetMspt() != 42 || b.GetPlayers() != 2 || b.GetSlots() != 20 {
		t.Errorf("lobby-b = %+v", b)
	}
	if b.GetAgeSeconds() != 90*60 {
		t.Errorf("age = %ds, want the pod's 5400", b.GetAgeSeconds())
	}
	if res.GetTotal().GetPods() != 2 {
		t.Errorf("a group's total covers its own pods: %d", res.GetTotal().GetPods())
	}
}

func TestStatusProxyTargets(t *testing.T) {
	res, err := status(t, allMeasured(), netstate.ForProxies, "gateway-a")
	if err != nil {
		t.Fatal(err)
	}
	in := res.GetInstances()
	if len(in) != 1 || !in[0].GetProxy() || !in[0].GetReady() || in[0].GetPlayers() != 6 {
		t.Fatalf("instances = %v, want gateway-a, ready, 6 players", in)
	}
	if in[0].GetUsage().GetCpuLimitMillicores() != 500 {
		t.Errorf("gateway-a limit = %d, want 500", in[0].GetUsage().GetCpuLimitMillicores())
	}
	res, err = status(t, allMeasured(), netstate.ForProxies, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetInstances()) != 1 || res.GetGroups()[0].GetKind() != agentpb.GroupState_PROXY {
		t.Errorf("the proxy group form = %+v", res)
	}
}

func TestStatusUnknownTarget(t *testing.T) {
	if _, err := status(t, allMeasured(), netstate.ForProxies, "nowhere"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("err = %v, want ErrUnknownTarget", err)
	}
}

func TestStatusWithoutMetrics(t *testing.T) {
	res, err := status(t, fixedMetrics{err: errors.New("the server could not find the requested resource")},
		netstate.ForProxies, "")
	if err != nil {
		t.Fatalf("a missing metrics API failed the request: %v", err)
	}
	if res.GetMetricsAvailable() {
		t.Error("metrics_available true without a metrics API")
	}
	if res.GetTotal().GetPodsMeasured() != 0 || res.GetTotal().GetCpuRequestedMillicores() != 1550 {
		t.Errorf("total = %+v, want requests but nothing measured", res.GetTotal())
	}
	if groupNamed(res, "lobby").GetLowestTps() != 16.5 {
		t.Error("TPS went missing with the metrics")
	}
}

func TestStatusCountsUnmeasuredPods(t *testing.T) {
	m := allMeasured()
	delete(m.usage, "lobby-b")
	res, err := status(t, m, netstate.ForProxies, "")
	if err != nil {
		t.Fatal(err)
	}
	if tot := res.GetTotal(); tot.GetPods() != 5 || tot.GetPodsMeasured() != 4 || tot.GetCpuUsedMillicores() != 570 {
		t.Errorf("total = %+v, want 4 of 5 measured and 570m used", tot)
	}
	if l := groupNamed(res, "lobby").GetUsage(); l.GetPods() != 2 || l.GetPodsMeasured() != 1 {
		t.Errorf("lobby usage = %+v, want 1 of 2 measured", l)
	}
}

func TestLowestTPSIgnoresUnreported(t *testing.T) {
	c, reg := network(t)
	if err := reg.ReportTicks("uid-lb", 0, 0); err != nil {
		t.Fatal(err)
	}
	res, err := Source{Reader: c, Agents: reg, Metrics: allMeasured(), Clock: func() time.Time { return t0 }}.
		Status(context.Background(), ns, netstate.ForProxies, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := groupNamed(res, "lobby").GetLowestTps(); got != 19.9 {
		t.Errorf("lowest TPS = %v, want 19.9: a server that reports none is not a server at 0", got)
	}
}

func TestStaleTicksAreDropped(t *testing.T) {
	c, _ := network(t)
	now := t0
	reg := agent.New(func() time.Time { return now }, 5*time.Second)
	reg.Connect("uid-la", agent.RoleServer)
	if err := reg.ReportPlayers("uid-la", 1, 20); err != nil {
		t.Fatal(err)
	}
	if err := reg.ReportTicks("uid-la", 19, 9); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(time.Minute)
	res, err := Source{Reader: c, Agents: reg, Metrics: allMeasured(), Clock: func() time.Time { return now }}.
		Status(context.Background(), ns, netstate.ForProxies, "lobby-a")
	if err != nil {
		t.Fatal(err)
	}
	if tps := res.GetInstances()[0].GetTps(); tps != 0 {
		t.Errorf("TPS = %v from a report a minute old, want 0 (not reported)", tps)
	}
}

type hangingMetrics struct{}

func (hangingMetrics) PodUsage(ctx context.Context, _ string) (map[string]Usage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStatusDoesNotWaitOnAHangingMetricsAPI(t *testing.T) {
	c, reg := network(t)
	start := time.Now()
	res, err := Source{Reader: c, Agents: reg, Metrics: hangingMetrics{}, Clock: func() time.Time { return t0 },
		MetricsTimeout: 50 * time.Millisecond}.Status(context.Background(), ns, netstate.ForProxies, "")
	if err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("waited %v on a metrics API that never answers", waited)
	}
	if res.GetMetricsAvailable() {
		t.Error("metrics_available true after the metrics call timed out")
	}
}

func TestStatusLogsWhyMetricsFailed(t *testing.T) {
	c, reg := network(t)
	var logged []string
	log := funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{})
	_, err := Source{Reader: c, Agents: reg, Clock: func() time.Time { return t0 }, Log: log,
		Metrics: fixedMetrics{err: errors.New("pods.metrics.k8s.io is forbidden")}}.
		Status(context.Background(), ns, netstate.ForProxies, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "forbidden") {
		t.Fatalf("logged %q, want one line carrying the metrics error", logged)
	}
}

func TestStatusNamesTheNode(t *testing.T) {
	res, err := status(t, allMeasured(), netstate.ForProxies, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range res.GetInstances() {
		if in.GetNode() != "node-1" {
			t.Errorf("%s node = %q, want node-1", in.GetName(), in.GetNode())
		}
	}
	res, err = status(t, allMeasured(), netstate.ForProxies, "gateway-a")
	if err != nil {
		t.Fatal(err)
	}
	if n := res.GetInstances()[0].GetNode(); n != "node-3" {
		t.Errorf("gateway-a node = %q, want node-3", n)
	}
}

func TestStatusCarriesThePlayableFigure(t *testing.T) {
	res, err := status(t, allMeasured(), netstate.ForProxies, "lobby-b")
	if err != nil {
		t.Fatal(err)
	}
	if n := res.GetInstances()[0].GetPlayableSlots(); n != 12 {
		t.Errorf("playable_slots = %d, want 12", n)
	}
	res, err = status(t, allMeasured(), netstate.ForProxies, "gateway-a")
	if err != nil {
		t.Fatal(err)
	}
	gw := res.GetInstances()[0]
	if gw.GetPlayableSlots() != gw.GetSlots() {
		t.Errorf("a proxy's playable = %d, want its slots %d", gw.GetPlayableSlots(), gw.GetSlots())
	}
}
