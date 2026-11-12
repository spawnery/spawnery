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
	"fmt"
	"os"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/agentendpoint"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

type replica struct {
	uid    types.UID
	cancel context.CancelFunc
	done   chan error
}

func TestAStandbyTakesOverTheAgentEndpoint(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.AgentServiceName, Namespace: ns},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Name: "agent", Port: agentserver.DefaultPort, Protocol: corev1.ProtocolTCP,
		}}},
	}
	if err := c.Create(ctx, svc); err != nil {
		t.Fatalf("create Service: %v", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	serving := make(chan struct{})
	close(serving)

	replicas := map[types.UID]*replica{}
	for i, pod := range []struct {
		name string
		uid  types.UID
	}{{"op-a", "uid-a"}, {"op-b", "uid-b"}} {
		name, uid := pod.name, pod.uid
		mgr, err := ctrl.NewManager(testenv.Config(t), managerOptions(managerFlags{
			leaderElect: true, operatorNamespace: ns, metricsAddr: "0", probeAddr: "0",
		}))
		if err != nil {
			t.Fatalf("new manager %s: %v", name, err)
		}
		direct, err := newDirectClient(testenv.Config(t), mgr)
		if err != nil {
			t.Fatalf("direct client %s: %v", name, err)
		}
		if err := mgr.Add(agentPublisher(direct, ns, agentendpoint.Identity{
			Name: name, Hostname: hostname, UID: uid, IPs: []string{fmt.Sprintf("10.0.0.%d", i+1)},
		}, serving, true)); err != nil {
			t.Fatalf("add publisher %s: %v", name, err)
		}

		mgrCtx, cancel := context.WithCancel(context.Background())
		r := &replica{uid: uid, cancel: cancel, done: make(chan error, 1)}
		go func() { r.done <- mgr.Start(mgrCtx) }()
		replicas[uid] = r
		t.Cleanup(func() {
			cancel()
			select {
			case <-r.done:
			case <-time.After(20 * time.Second):
				t.Errorf("manager %s did not stop within 20s", name)
			}
		})
	}

	named := func() (types.UID, string) {
		var list discoveryv1.EndpointSliceList
		if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return "", err.Error()
		}
		if len(list.Items) != 1 {
			return "", fmt.Sprintf("%d slices", len(list.Items))
		}
		eps := list.Items[0].Endpoints
		if len(eps) != 1 || eps[0].TargetRef == nil {
			return "", fmt.Sprintf("slice %s has endpoints %+v", list.Items[0].Name, eps)
		}
		return eps[0].TargetRef.UID, ""
	}

	var leader types.UID
	within(t, 20*time.Second, "exactly one slice naming one of the two replicas", func() (bool, string) {
		uid, detail := named()
		if _, ok := replicas[uid]; !ok {
			return false, detail + string(uid)
		}
		leader = uid
		return true, ""
	})

	first := replicas[leader]
	first.cancel()
	within(t, 10*time.Second, "the slice to name the standby after the leader stopped", func() (bool, string) {
		uid, detail := named()
		if uid == "" {
			return false, detail
		}
		return uid != leader, "the slice still names " + string(uid)
	})

	select {
	case err := <-first.done:
		if err != nil {
			t.Errorf("the stopped leader's manager returned %v, want nil", err)
		}
		first.done <- err
	case <-time.After(10 * time.Second):
		t.Error("the stopped leader's manager had not returned 10s after its context was cancelled")
	}
}

func TestWithoutLeaderElectionTheAgentEndpointIsPublished(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.AgentServiceName, Namespace: ns},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Name: "agent", Port: agentserver.DefaultPort, Protocol: corev1.ProtocolTCP,
		}}},
	}
	if err := c.Create(ctx, svc); err != nil {
		t.Fatalf("create Service: %v", err)
	}

	mgr, err := ctrl.NewManager(testenv.Config(t), managerOptions(managerFlags{
		leaderElect: false, operatorNamespace: ns, metricsAddr: "0", probeAddr: "0",
	}))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	direct, err := newDirectClient(testenv.Config(t), mgr)
	if err != nil {
		t.Fatalf("direct client: %v", err)
	}
	serving := make(chan struct{})
	close(serving)
	if err := mgr.Add(agentPublisher(direct, ns, agentendpoint.Identity{
		Name: "op-a", Hostname: "op-a", UID: "uid-a", IPs: []string{"10.0.0.1"},
	}, serving, false)); err != nil {
		t.Fatalf("add publisher: %v", err)
	}
	mgrCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("manager did not stop within 20s")
		}
	})

	within(t, 10*time.Second, "a slice naming the only replica, with no Lease anywhere", func() (bool, string) {
		var list discoveryv1.EndpointSliceList
		if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return false, err.Error()
		}
		if len(list.Items) != 1 {
			return false, fmt.Sprintf("%d slices", len(list.Items))
		}
		eps := list.Items[0].Endpoints
		if len(eps) != 1 || eps[0].TargetRef == nil || eps[0].TargetRef.UID != "uid-a" {
			return false, fmt.Sprintf("endpoints %+v", eps)
		}
		return true, ""
	})
	var leases coordinationv1.LeaseList
	if err := c.List(ctx, &leases, client.InNamespace(ns)); err != nil {
		t.Fatalf("list Leases: %v", err)
	}
	if len(leases.Items) != 0 {
		t.Errorf("%d Leases in the namespace with leader election off, want none: the test "+
			"would not show that the publisher needs none", len(leases.Items))
	}
}
