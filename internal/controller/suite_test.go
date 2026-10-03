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
	"fmt"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func ctrlclientInNamespace(ns string) client.ListOption { return client.InNamespace(ns) }

// createOrderRecorder records, in commit order, the kind and name of every
// object it created, so a test can assert creation order and not only the
// end state.
type createOrderRecorder struct {
	client.Client
	mu    sync.Mutex
	order []string
}

func (r *createOrderRecorder) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := r.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	r.mu.Lock()
	r.order = append(r.order, fmt.Sprintf("%T/%s", obj, obj.GetName()))
	r.mu.Unlock()
	return nil
}

func (r *createOrderRecorder) indexOf(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, entry := range r.order {
		if strings.HasPrefix(entry, prefix) {
			return i
		}
	}
	return -1
}

// agentRoleServer avoids importing the agent package into every test file.
func agentRoleServer() agent.Role { return agent.RoleServer }

var intstrInt = intstr.Int

func hasCondition(conds []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == status && c.Reason == reason {
			return true
		}
	}
	return false
}

// bringUpNamed walks a created server into Ready and returns its pod UID. It
// takes three passes: create the pod, move to Starting, then pass the ready
// gate, which needs both the probe and the agent.
func bringUpNamed(t *testing.T, f *fixture, name string) string {
	t.Helper()
	f.reconcile(name)

	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("reconcile did not create the pod for %s", name)
	}
	uid := string(pod.UID)

	f.setPodRunning(name, false)
	f.reconcile(name)

	f.setPodRunning(name, true)
	f.agents.Connect(uid, agentRoleServer())
	f.agents.MarkReady(uid)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(name)

	if got := f.server(name).Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase of %s = %q, want Ready", name, got)
	}
	return uid
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type recordingRegistrar struct {
	registered   []string
	deregistered []string
	drained      []string

	// onRegister runs inside Register, so a test can observe what was already
	// durable at the moment the proxies were told.
	onRegister func(*spawneryv1alpha1.Server) error
}

func (r *recordingRegistrar) Register(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.registered = append(r.registered, s.Name)
	if r.onRegister != nil {
		return r.onRegister(s)
	}
	return nil
}

func (r *recordingRegistrar) Deregister(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.deregistered = append(r.deregistered, s.Name)
	return nil
}

func (r *recordingRegistrar) Drain(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.drained = append(r.drained, s.Name)
	return nil
}

// fixture holds one test's envtest namespace and the two clients that reach
// it. c is admin and serves only the test harness. rc holds exactly the
// ClusterRole generated into config/rbac/role.yaml; every reconciler and
// Bootstrapper under test gets rc, so a missing verb fails here rather than
// on a cluster.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	c         client.Client
	rc        client.Client
	ns        string
	clock     *testClock
	agents    *agent.Registry
	registrar *recordingRegistrar
	proxies   *recordingFleet
	reconc    *ServerReconciler
	network   *spawneryv1alpha1.Network
	group     *spawneryv1alpha1.ServerGroup
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	rc := testenv.RestrictedClient(t)
	ns := testenv.Namespace(t, ctx, c)

	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}
	agents := agent.New(clock.Now, 5*time.Second, start)
	registrar := &recordingRegistrar{}
	proxies := &recordingFleet{}

	// A stand-in CA; agentchannel_envtest_test.go replaces it with the bundle
	// its gRPC service actually serves.
	bootstrap := &Bootstrapper{
		Client: rc, Reader: rc,
		CA: func() []byte { return []byte("test-ca") },
	}

	f := &fixture{
		t: t, ctx: ctx, c: c, rc: rc, ns: ns,
		clock: clock, agents: agents, registrar: registrar, proxies: proxies,
		reconc: &ServerReconciler{
			Client:               rc,
			Scheme:               testenv.Scheme(t),
			Recorder:             newRecorder(),
			Agents:               agents,
			Clock:                clock.Now,
			StartupDeadline:      5 * time.Minute,
			PlayerStatusInterval: 30 * time.Second,
			Registrar:            registrar,
			Bootstrap:            bootstrap,
			AgentEndpoint:        "spawnery-operator.spawnery-system.svc:9443",
		},
	}

	f.network = &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			// The whole port range, so the expose tests that use HostPort
			// keep testing exposure; scheduling_test.go narrows it.
			Scheduling: &spawneryv1alpha1.SchedulingPolicy{
				HostPortRange: &spawneryv1alpha1.PortRange{Min: 1, Max: 65535},
			},
		},
	}
	if err := c.Create(ctx, f.network); err != nil {
		t.Fatalf("create Network: %v", err)
	}
	// The fixture's Network is trivially the namespace owner; accept it once
	// so tests need not drive the Network controller themselves.
	netReconciler := &NetworkReconciler{
		Client:       rc,
		Scheme:       testenv.Scheme(t),
		Recorder:     newRecorder(),
		SecretReader: c,
		Bootstrap:    bootstrap,
	}
	if _, err := netReconciler.Reconcile(ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.network.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("accept fixture network: %v", err)
	}

	// envtest runs no namespace controller, and a NodePort is cluster-scoped:
	// delete the Service so the next test can reuse its port.
	t.Cleanup(func() {
		svcs := &corev1.ServiceList{}
		if err := c.List(ctx, svcs, client.InNamespace(ns)); err != nil {
			return
		}
		for i := range svcs.Items {
			_ = c.Delete(ctx, &svcs.Items[i])
		}
	})

	f.group = &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:                          spawneryv1alpha1.ServerGroupEphemeral,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    100,
			TerminationGracePeriodSeconds: 60,
			FailedRetentionSeconds:        3600,
			Drain:                         &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 60},
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40,
			},
		},
	}
	if err := c.Create(ctx, f.group); err != nil {
		t.Fatalf("create ServerGroup: %v", err)
	}

	return f
}

func (f *fixture) createServer(name string) *spawneryv1alpha1.Server {
	f.t.Helper()
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       f.group.Name,
				UID:        f.group.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			GroupGeneration: f.group.Generation,
			PodHash:         f.desiredPodHash(),
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		f.t.Fatalf("create Server: %v", err)
	}
	return srv
}

// desiredPodHash is what the reconciler would stamp on a server it created
// for this group right now. staleSpec adopts an empty hash, so a fixture
// server without it could never be made stale by a spec change.
func (f *fixture) desiredPodHash() string {
	f.t.Helper()
	configValues, err := serverConfigValues(f.group)
	if err != nil {
		f.t.Fatalf("server config values: %v", err)
	}
	hash, err := podspec.DesiredServerHash(f.network, f.group, configValues)
	if err != nil {
		f.t.Fatalf("desired server hash: %v", err)
	}
	return hash
}

func (f *fixture) reconcile(name string) {
	f.t.Helper()
	_, err := f.reconc.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	})
	if err != nil {
		f.t.Fatalf("reconcile %s: %v", name, err)
	}
}

func (f *fixture) server(name string) *spawneryv1alpha1.Server {
	f.t.Helper()
	srv := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, srv); err != nil {
		f.t.Fatalf("get Server %s: %v", name, err)
	}
	return srv
}

// pod re-reads the pod of a server. found is false once it is gone, including
// a pod that only carries a deletion timestamp: envtest runs no kubelet.
func (f *fixture) pod(name string) (*corev1.Pod, bool) {
	f.t.Helper()
	pod := &corev1.Pod{}
	err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, pod)
	if err != nil {
		return nil, false
	}
	if !pod.DeletionTimestamp.IsZero() {
		return pod, false
	}
	return pod, true
}

// setPodRunning fakes what a kubelet would do: envtest runs no nodes.
func (f *fixture) setPodRunning(name string, ready bool) {
	f.t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		f.t.Fatalf("pod %s not found", name)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.42.3.17"
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: cond,
		LastTransitionTime: metav1.NewTime(f.clock.Now()),
	}}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		f.t.Fatalf("update pod status: %v", err)
	}
}

// bindPodToNode does what a scheduler would. pod.spec.nodeName cannot be set
// by Update, so the binding subresource is the only way to place a pod.
func (f *fixture) bindPodToNode(t *testing.T, pod *corev1.Pod, nodeName string) {
	t.Helper()
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		Target:     corev1.ObjectReference{Kind: "Node", Name: nodeName},
	}
	if err := f.c.SubResource("binding").Create(f.ctx, pod, binding); err != nil {
		t.Fatalf("bind pod %s to node %s: %v", pod.Name, nodeName, err)
	}
}

// ensureNode creates a Node, cordoned or not. Nodes are cluster-scoped, so
// they need a unique name and their own cleanup.
func (f *fixture) ensureNode(t *testing.T, name string, unschedulable bool) *corev1.Node {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrUpdate(f.ctx, f.c, node, func() error {
		node.Spec.Unschedulable = unschedulable
		return nil
	})
	if err != nil {
		t.Fatalf("ensure node %s: %v", name, err)
	}
	t.Cleanup(func() { _ = f.c.Delete(f.ctx, node) })
	return node
}

// nonBlockingRecorder replaces events.FakeRecorder, which blocks inside
// Eventf once its per-call-site buffer is full, so a test hangs instead of
// failing. The format string is FakeRecorder's, dropping `action` as it does.
type nonBlockingRecorder struct {
	mu     sync.Mutex
	events []string
}

func newRecorder() *nonBlockingRecorder { return &nonBlockingRecorder{} }

func (r *nonBlockingRecorder) Eventf(
	regarding runtime.Object, related runtime.Object,
	eventtype, reason, action, note string, args ...interface{},
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(eventtype+" "+reason+" "+note, args...))
}

func (r *nonBlockingRecorder) WithLogger(klog.Logger) events.EventRecorderLogger { return r }
