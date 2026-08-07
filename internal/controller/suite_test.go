/*
Copyright The Spawnery Authors.

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
	"os"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

// containsString is the test-side spelling of the finalizer check.
func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// ctrlclientInNamespace is a shorthand for the list option.
func ctrlclientInNamespace(ns string) client.ListOption { return client.InNamespace(ns) }

// agentRoleServer avoids importing the agent package into every test file.
func agentRoleServer() agent.Role { return agent.RoleServer }

// intstrInt is the IntOrString kind an absolute PDB value must have.
var intstrInt = intstr.Int

// hasCondition reports whether the list carries the given condition.
func hasCondition(conds []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == status && c.Reason == reason {
			return true
		}
	}
	return false
}

// bringUpNamed walks an already-created server into Ready and returns the pod
// UID the agent registry is keyed on. Reaching Ready takes three passes: the
// first creates the pod, the second sees it running and moves to Starting, and
// only the third can pass the ready gate, which needs both the probe and the
// agent.
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

// testClock is a hand-cranked clock shared by the controller tests.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// recordingRegistrar remembers the calls the controller made.
type recordingRegistrar struct {
	registered   []string
	deregistered []string
	drained      []string
}

func (r *recordingRegistrar) Register(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.registered = append(r.registered, s.Name)
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

// fixture is one isolated namespace with a network, a group and the wired
// Server reconciler.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	c         client.Client
	ns        string
	clock     *testClock
	agents    *agent.Registry
	registrar *recordingRegistrar
	reconc    *ServerReconciler
	network   *spawneryv1alpha1.Network
	group     *spawneryv1alpha1.ServerGroup
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}
	agents := agent.New(clock.Now, 5*time.Second, start)
	registrar := &recordingRegistrar{}

	f := &fixture{
		t: t, ctx: ctx, c: c, ns: ns,
		clock: clock, agents: agents, registrar: registrar,
		reconc: &ServerReconciler{
			Client:               c,
			Scheme:               testenv.Scheme(t),
			Recorder:             record.NewFakeRecorder(100),
			Agents:               agents,
			Clock:                clock.Now,
			StartupDeadline:      5 * time.Minute,
			PlayerStatusInterval: 30 * time.Second,
			Registrar:            registrar,
		},
	}

	f.network = &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := c.Create(ctx, f.network); err != nil {
		t.Fatalf("create Network: %v", err)
	}
	// The fixture's Network is the only one in this namespace at this point,
	// so it is trivially the namespace owner; accept it once here so every
	// test gets a usable Network without having to drive the Network
	// controller itself. Tests that specifically exercise rejection or
	// recovery construct their own NetworkReconciler and reconcile again.
	netReconciler := &NetworkReconciler{
		Client:   c,
		Scheme:   testenv.Scheme(t),
		Recorder: record.NewFakeRecorder(100),
		Clock:    clock.Now,
	}
	if _, err := netReconciler.Reconcile(ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.network.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("accept fixture network: %v", err)
	}

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

// createServer adds one Server owned by the fixture's group.
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
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		f.t.Fatalf("create Server: %v", err)
	}
	return srv
}

// reconcile runs the Server reconciler once.
func (f *fixture) reconcile(name string) {
	f.t.Helper()
	_, err := f.reconc.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	})
	if err != nil {
		f.t.Fatalf("reconcile %s: %v", name, err)
	}
}

// server re-reads a Server.
func (f *fixture) server(name string) *spawneryv1alpha1.Server {
	f.t.Helper()
	srv := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, srv); err != nil {
		f.t.Fatalf("get Server %s: %v", name, err)
	}
	return srv
}

// pod re-reads the pod of a server. found is false once it is gone — which
// includes a pod that only carries a deletion timestamp, because envtest runs
// no kubelet to finish the job.
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
