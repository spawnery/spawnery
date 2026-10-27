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
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

type noProxies struct{}

func (noProxies) SetReady(context.Context, string, bool) error { return nil }

// No other envtest objects live in this binary, so a cluster-wide manager here
// reconciles only what this test creates.
func TestTheProductionManagerReconcilesEveryNamespace(t *testing.T) {
	c, ctx := testenv.Client(t)
	namespaces := []string{testenv.Namespace(t, ctx, c), testenv.Namespace(t, ctx, c)}

	opts := managerOptions(managerFlags{metricsAddr: "0", probeAddr: "0", operatorNamespace: namespaces[0]})
	if len(opts.Cache.DefaultNamespaces) != 0 {
		t.Fatalf("cache namespaces = %v with no --namespace, want the whole cluster", opts.Cache.DefaultNamespaces)
	}
	mgr, err := ctrl.NewManager(testenv.Config(t), opts)
	if err != nil {
		t.Fatalf("new manager: %v", err)
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
		AgentEndpoint:     agentEndpoint(namespaces[0]),
		OperatorNamespace: namespaces[0],
		Proxies:           noProxies{},
	}); err != nil {
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

	for _, ns := range namespaces {
		network := &spawneryv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
			Spec: spawneryv1alpha1.NetworkSpec{
				ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			},
		}
		if err := c.Create(ctx, network); err != nil {
			t.Fatalf("create network in %s: %v", ns, err)
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
		if err := c.Create(ctx, group); err != nil {
			t.Fatalf("create server group in %s: %v", ns, err)
		}
	}

	for _, ns := range namespaces {
		within(t, 60*time.Second, "the network in "+ns+" to be accepted and its group to get a pod", func() (bool, string) {
			var network spawneryv1alpha1.Network
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "production"}, &network); err != nil {
				return false, err.Error()
			}
			if !meta.IsStatusConditionTrue(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
				return false, "network not accepted"
			}
			var servers spawneryv1alpha1.ServerList
			if err := c.List(ctx, &servers, client.InNamespace(ns)); err != nil {
				return false, err.Error()
			}
			if len(servers.Items) == 0 {
				return false, "no server"
			}
			var pod corev1.Pod
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: servers.Items[0].Name}, &pod); err != nil {
				return false, "server " + servers.Items[0].Name + " has no pod: " + err.Error()
			}
			return true, ""
		})
	}

	unlabelled := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ours", Namespace: namespaces[1]},
		Data:       map[string]string{"k": "v"},
	}
	if err := c.Create(ctx, unlabelled); err != nil {
		t.Fatalf("create ConfigMap: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), unlabelled) })
	labelled := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ours", Namespace: namespaces[1],
			Labels: map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue}},
	}
	if err := c.Create(ctx, labelled); err != nil {
		t.Fatalf("create ConfigMap: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), labelled) })
	within(t, 30*time.Second, "the manager's cache to see the labelled ConfigMap created after the other", func() (bool, string) {
		err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(labelled), &corev1.ConfigMap{})
		return err == nil, "Get: " + errString(err)
	})
	err = mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get of an unlabelled ConfigMap through the manager's cache = %v, want NotFound: "+
			"the cache holds only the ConfigMaps this operator labels", err)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "images-" + namespaces[0]}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatalf("create Node: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), node) })
	node.Status.Images = []corev1.ContainerImage{{Names: []string{"ghcr.io/spawnery/paper:1.21.4-0.1.0"}, SizeBytes: 1 << 30}}
	if err := c.Status().Update(ctx, node); err != nil {
		t.Fatalf("set Node images: %v", err)
	}
	within(t, 30*time.Second, "the manager's cache to see the Node without its images", func() (bool, string) {
		var cached corev1.Node
		if err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(node), &cached); err != nil {
			return false, err.Error()
		}
		if cached.ResourceVersion != node.ResourceVersion {
			return false, "cache at resourceVersion " + cached.ResourceVersion
		}
		if len(cached.Status.Images) != 0 {
			t.Fatalf("cached Node carries %d images, want none: the transform is off", len(cached.Status.Images))
		}
		return true, ""
	})
}

func errString(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func within(t *testing.T, deadline time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	last := "nothing observed yet"
	for stop := time.Now().Add(deadline); time.Now().Before(stop); time.Sleep(250 * time.Millisecond) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
	}
	t.Fatalf("timed out after %s waiting for %s; last seen: %s", deadline, what, last)
}
