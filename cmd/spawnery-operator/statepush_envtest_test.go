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

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/statepush"
	"github.com/spawnery/spawnery/internal/testenv"
)

// pictureRecorder stands in for the proxies' fleet: each push builds the
// picture from the manager's cache, as the fleet does, and keeps the entry of
// the servers in it.
type pictureRecorder struct {
	source netstate.Source

	mu       sync.Mutex
	pictures [][]*agentpb.ServerState
}

func (p *pictureRecorder) SendState(ctx context.Context, namespace string) {
	state, err := p.source.Build(ctx, namespace, netstate.ForProxies)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pictures = append(p.pictures, state.GetServers())
}

// seen reports whether a picture held a server for which match is true.
func (p *pictureRecorder) seen(match func(*agentpb.ServerState) bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, servers := range p.pictures {
		for _, s := range servers {
			if match(s) {
				return true
			}
		}
	}
	return false
}

// The push is built from the cache the reconcile ran against, so the picture
// it carries already shows the change that triggered it. Nothing here waits for
// the 30 s resync: the timeouts are far below it.
func TestAServersLifecycleReachesTheProxiesWithoutWaitingForTheResync(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	opts := managerOptions(managerFlags{metricsAddr: "0", probeAddr: "0", watchNamespace: ns, operatorNamespace: ns})
	opts.Controller = config.Controller{SkipNameValidation: ptr.To(true)}
	mgr, err := ctrl.NewManager(testenv.Config(t), opts)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	pictures := &pictureRecorder{source: netstate.Source{
		Reader: mgr.GetClient(),
		Agents: agent.New(time.Now, 5*time.Second, time.Now()),
	}}
	notifier := statepush.New(statepush.Options{Proxies: pictures, Interval: 100 * time.Millisecond})
	if err := mgr.Add(notifier); err != nil {
		t.Fatalf("add the notifier: %v", err)
	}
	if err := controller.SetupAll(mgr, controller.Options{
		Agents:               agent.New(time.Now, 5*time.Second, time.Now()),
		Clock:                time.Now,
		StartupDeadline:      5 * time.Minute,
		PlayerStatusInterval: 30 * time.Second,
		OrphanInterval:       time.Minute,
		Registrar:            controller.NoopRegistrar{},
		Bootstrapper: &controller.Bootstrapper{
			Client: mgr.GetClient(), Reader: mgr.GetAPIReader(),
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint:     agentEndpoint(ns),
		OperatorNamespace: ns,
		Proxies:           noProxies{},
		State:             notifier,
	}); err != nil {
		t.Fatalf("SetupAll: %v", err)
	}

	mgrCtx, cancel := context.WithCancel(context.Background())
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-mgrDone:
		case <-time.After(20 * time.Second):
			t.Error("manager did not stop within 20s of context cancellation")
		}
	})

	network := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := c.Create(ctx, network); err != nil {
		t.Fatalf("create network: %v", err)
	}
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: network.Name},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 1, SpareSlots: 10},
		},
	}
	if err := c.Create(ctx, group); err != nil {
		t.Fatalf("create server group: %v", err)
	}

	var pod corev1.Pod
	within(t, 60*time.Second, "the group's server to get a pod", func() (bool, string) {
		var servers spawneryv1alpha1.ServerList
		if err := c.List(ctx, &servers, client.InNamespace(ns)); err != nil || len(servers.Items) == 0 {
			return false, "no server: " + errString(err)
		}
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: servers.Items[0].Name}, &pod)
		return err == nil, "no pod: " + errString(err)
	})

	within(t, 10*time.Second, "the proxies to be shown the new server", func() (bool, string) {
		return pictures.seen(func(s *agentpb.ServerState) bool { return s.GetName() == pod.Name }), "no picture holds it"
	})

	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "0/2 nodes are available: 2 Insufficient memory.",
	}}
	if err := c.Status().Update(ctx, &pod); err != nil {
		t.Fatalf("mark the pod unschedulable: %v", err)
	}
	within(t, 10*time.Second, "the proxies to be told the server cannot be placed", func() (bool, string) {
		return pictures.seen(func(s *agentpb.ServerState) bool {
			return s.GetName() == pod.Name && s.GetUnschedulable()
		}), "no picture says so"
	})

	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: ns},
		Target:     corev1.ObjectReference{Kind: "Node", Name: "node-1"},
	}
	if err := c.SubResource("binding").Create(ctx, &pod, binding); err != nil {
		t.Fatalf("bind the pod: %v", err)
	}
	within(t, 10*time.Second, "the proxies to be told which node the server runs on", func() (bool, string) {
		return pictures.seen(func(s *agentpb.ServerState) bool {
			return s.GetName() == pod.Name && s.GetNode() == "node-1" && !s.GetUnschedulable()
		}), "no picture names the node"
	})
}
