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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestSetupAllRegistersEveryController(t *testing.T) {
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}

	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:         testenv.Scheme(t),
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	opts := Options{
		Agents:               agent.New(clock.Now, 5*time.Second, start),
		Clock:                clock.Now,
		StartupDeadline:      5 * time.Minute,
		PlayerStatusInterval: 30 * time.Second,
		OrphanInterval:       time.Minute,
		Registrar:            NoopRegistrar{},
		Bootstrapper: &Bootstrapper{
			Client: mgr.GetClient(), Reader: mgr.GetAPIReader(),
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint: "spawnery-operator.spawnery-system.svc:9443",
		Proxies:       &recordingFleet{},
	}
	if err := SetupAll(mgr, opts); err != nil {
		t.Fatalf("SetupAll: %v", err)
	}

	// Registering the same controllers twice must fail: controller-runtime
	// rejects duplicate names. That proves SetupAll really registered them.
	if err := SetupAll(mgr, opts); err == nil {
		t.Fatal("SetupAll succeeded twice, so it registered nothing the first time")
	}
}

// Without the Bootstrapper the Server controller would panic on the first pod
// it creates — in a reconcile goroutine, long after start. Refusing at setup
// turns that into a startup error nobody can miss.
func TestSetupAllRefusesWithoutABootstrapper(t *testing.T) {
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}

	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:         testenv.Scheme(t),
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
		// So the only possible reason to fail is the missing Bootstrapper and
		// not a controller name this test binary already used.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	err = SetupAll(mgr, Options{
		Agents:               agent.New(clock.Now, 5*time.Second, start),
		Clock:                clock.Now,
		StartupDeadline:      5 * time.Minute,
		PlayerStatusInterval: 30 * time.Second,
		OrphanInterval:       time.Minute,
		Registrar:            NoopRegistrar{},
		AgentEndpoint:        "spawnery-operator.spawnery-system.svc:9443",
		// Proxies is set so the only possible reason to fail is the missing
		// Bootstrapper, not the (equally refused) missing Proxies.
		Proxies: &recordingFleet{},
	})
	if err == nil {
		t.Fatal("SetupAll accepted a nil Bootstrapper")
	}
	if !strings.Contains(err.Error(), "bootstrapper") {
		t.Errorf("error = %q, want it to name the missing bootstrapper", err)
	}
}

// Without Proxies the ProxyGroup controller would panic the first time it
// tries to assert readiness on a pod — in a reconcile goroutine, long after
// start, exactly like the missing-Bootstrapper case above. Refusing at setup
// turns that into a startup error nobody can miss.
func TestSetupAllRefusesWithoutProxies(t *testing.T) {
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}

	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:         testenv.Scheme(t),
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
		// So the only possible reason to fail is the missing Proxies and not a
		// controller name this test binary already used.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	err = SetupAll(mgr, Options{
		Agents:               agent.New(clock.Now, 5*time.Second, start),
		Clock:                clock.Now,
		StartupDeadline:      5 * time.Minute,
		PlayerStatusInterval: 30 * time.Second,
		OrphanInterval:       time.Minute,
		Registrar:            NoopRegistrar{},
		Bootstrapper: &Bootstrapper{
			Client: mgr.GetClient(), Reader: mgr.GetAPIReader(),
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint: "spawnery-operator.spawnery-system.svc:9443",
	})
	if err == nil {
		t.Fatal("SetupAll accepted a nil Proxies")
	}
	if !strings.Contains(err.Error(), "proxies") {
		t.Errorf("error = %q, want it to name the missing proxies", err)
	}
}

// TestManagerReconcilesEndToEnd is the one thing no earlier task has proven:
// that a real, running manager — leader election on, exactly as the binary
// starts it — turns a Network and a ServerGroup into Servers and pods without
// any test ever calling Reconcile itself. Task 12's brief called for a k3d
// smoke test instead, but this environment has no container runtime, so this
// test is the substitute against the envtest control plane.
//
// It also stands in for "does a single-replica deployment start promptly with
// leader election on": the manager here has nobody to contend the lease with,
// so if election itself were slow or broken, mgr.Elected() would not close
// within the deadline below and the test would fail on that wait, not later.
func TestManagerReconcilesEndToEnd(t *testing.T) {
	c, setupCtx := testenv.Client(t)
	ns := testenv.Namespace(t, setupCtx, c)

	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}

	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:                  testenv.Scheme(t),
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		LeaderElection:          true,
		LeaderElectionNamespace: ns,
		LeaderElectionID:        "spawnery-e2e-test",
		// controller-runtime tracks controller names in a process-global set to
		// catch metric collisions. TestSetupAllRegistersEveryController already
		// registered "network"/"servergroup"/"server"/"proxygroup" once in this
		// test binary; this manager is a separate instance with its own metrics,
		// so the collision the check guards against does not apply here.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	opts := Options{
		Agents:               agent.New(clock.Now, 5*time.Second, start),
		Clock:                clock.Now,
		StartupDeadline:      5 * time.Minute,
		PlayerStatusInterval: 30 * time.Second,
		OrphanInterval:       time.Minute,
		Registrar:            NoopRegistrar{},
		Bootstrapper: &Bootstrapper{
			Client: mgr.GetClient(), Reader: mgr.GetAPIReader(),
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint: "spawnery-operator.spawnery-system.svc:9443",
		Proxies:       &recordingFleet{},
	}
	if err := SetupAll(mgr, opts); err != nil {
		t.Fatalf("SetupAll: %v", err)
	}

	mgrCtx, cancel := context.WithCancel(context.Background())
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-mgrDone:
			if err != nil {
				t.Errorf("manager exited with an error after context cancellation: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("manager did not stop within 20s of context cancellation")
		}
	})

	// A single-replica manager has no competition for the lease: election has
	// to complete quickly, or something about the wiring is wrong.
	select {
	case <-mgr.Elected():
	case <-time.After(15 * time.Second):
		t.Fatal("manager was not elected leader within 15s")
	}

	network := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := c.Create(setupCtx, network); err != nil {
		t.Fatalf("create network: %v", err)
	}

	// Wait for the Network to be Accepted before creating the group: the group
	// controller only sizes itself against an already-accepted Network, and
	// otherwise falls back to its own 30s retry, which this test does not need
	// to exercise.
	networkAccepted := false
	for i := 0; i < 40; i++ {
		var got spawneryv1alpha1.Network
		if err := c.Get(setupCtx, types.NamespacedName{Name: network.Name, Namespace: ns}, &got); err != nil {
			t.Fatalf("get network: %v", err)
		}
		if meta.IsStatusConditionTrue(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
			networkAccepted = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !networkAccepted {
		t.Fatal("running manager never accepted the network, gave up after 20s")
	}

	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: network.Name},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
		},
	}
	if err := c.Create(setupCtx, group); err != nil {
		t.Fatalf("create server group: %v", err)
	}

	// Loop-driven, not a single sleep: the manager reacts to events on its own
	// schedule, and the resync interval alone can take several seconds.
	var servers spawneryv1alpha1.ServerList
	found := false
	for i := 0; i < 60; i++ {
		if err := c.List(setupCtx, &servers, client.InNamespace(ns)); err != nil {
			t.Fatalf("list servers: %v", err)
		}
		if len(servers.Items) == 1 {
			found = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatalf("running manager never created a Server for the group, got %d after 30s", len(servers.Items))
	}
	srv := servers.Items[0]
	if srv.Spec.GroupRef.Name != group.Name {
		t.Fatalf("server group ref = %q, want %q", srv.Spec.GroupRef.Name, group.Name)
	}

	podFound := false
	for i := 0; i < 60; i++ {
		var pod corev1.Pod
		err := c.Get(setupCtx, types.NamespacedName{Name: srv.Name, Namespace: ns}, &pod)
		if err == nil {
			podFound = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !podFound {
		t.Fatal("running manager never created a pod for the server, gave up after 30s")
	}
}
