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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

func drainOneProxy(t *testing.T, f *fixture, r *ProxyGroupReconciler, mutate ...func(*spawneryv1alpha1.ProxyGroup)) corev1.Pod {
	t.Helper()
	f.createProxyGroup("gateway", mutate...)
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	surplus := pods[1]
	f.reportProxyPlayers(t, surplus, 3)
	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	return surplus
}

func TestADrainingProxyWithPlayersIsNotDeletedAtTheDrainTimeout(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	surplus := drainOneProxy(t, f, r)

	f.clock.Advance(time.Hour)
	f.reportProxyPlayers(t, surplus, 3)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 2 {
		t.Fatalf("proxy pods = %d, want 2: a proxy with players waits", got)
	}
}

func TestADrainingProxyWithAnUnknownCountStillHasADeadline(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
	surplus := drainOneProxy(t, f, r)

	f.agents.Disconnect(string(surplus.UID))
	f.clock.Advance(time.Hour)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Fatalf("proxy pods = %d, want 1: an unknown count keeps its deadline", got)
	}
	if !containsEvent(drainEvents(rec), "ProxyDrainTimeout") {
		t.Error("no ProxyDrainTimeout event for the proxy deleted at its deadline")
	}
}

func TestMaxStaleSecondsBoundsAProxyDrain(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	surplus := drainOneProxy(t, f, r, func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Update = &spawneryv1alpha1.ProxyUpdateSpec{MaxStaleSeconds: 600}
	})

	f.clock.Advance(11 * time.Minute)
	f.reportProxyPlayers(t, surplus, 3)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Fatalf("proxy pods = %d, want 1: maxStaleSeconds has passed", got)
	}
}

func TestARetiredProxyIsReplacedAndGoesOnceEmpty(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
		f.reportProxyPlayers(t, pods[i], 2)
	}
	target := pods[0]
	patch := client.MergeFrom(target.DeepCopy())
	target.Annotations = map[string]string{podspec.AnnotationRetireRequested: "2026-09-26T12:00:00Z"}
	if err := f.c.Patch(f.ctx, &target, patch); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")
	now := f.proxyPods("gateway")
	if len(now) != 3 {
		t.Fatalf("proxy pods = %d, want 3: a replacement before the retired one drains", len(now))
	}
	for i := range now {
		if now[i].Name != target.Name && now[i].Name != pods[1].Name {
			f.markProxyPodReady(t, &now[i])
			f.reportProxyPlayers(t, now[i], 0)
		}
	}
	f.reconcileProxyGroup(r, "gateway")
	ev := drainEvents(rec)
	if !containsEvent(ev, "ProxyRetiring") || !strings.Contains(strings.Join(ev, "\n"), "retire requested") {
		t.Errorf("events = %v, want ProxyRetiring naming the retire request", ev)
	}

	f.reportProxyPlayers(t, target, 0)
	f.reconcileProxyGroup(r, "gateway")
	for _, p := range f.proxyPods("gateway") {
		if p.Name == target.Name {
			t.Fatal("the retired proxy is still there after it ran empty")
		}
	}
	if !containsEvent(drainEvents(rec), "ProxyStopped") {
		t.Error("no ProxyStopped event for the proxy that ran empty")
	}
}

func TestAProxyThatPassesTheReadyGateIsAnnouncedOnce(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) { g.Spec.Replicas = 1 })
	f.reconcileProxyGroup(r, "gateway")
	pod := f.proxyPods("gateway")[0]
	f.markProxyPodReady(t, &pod)

	f.reconcileProxyGroup(r, "gateway")
	f.reconcileProxyGroup(r, "gateway")
	started := 0
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, "ProxyStarted") {
			started++
		}
	}
	if started != 1 {
		t.Errorf("ProxyStarted events = %d, want exactly 1", started)
	}
	if f.proxyPods("gateway")[0].Annotations[podspec.AnnotationProxyReadySince] == "" {
		t.Error("the proxy carries no ready-since")
	}
}
