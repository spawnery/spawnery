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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// bringUpReady walks a fresh server all the way into phase Ready and returns
// the pod UID the agent registry is keyed on.
func bringUpReady(t *testing.T, f *fixture, name string) string {
	t.Helper()
	f.createServer(name)
	f.reconcile(name)

	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("reconcile did not create the pod for %s", name)
	}
	uid := string(pod.UID)

	f.setPodRunning(name, false)
	f.reconcile(name)

	f.setPodRunning(name, true)
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(name)

	if got := f.server(name).Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q, want Ready", got)
	}
	return uid
}

func TestReconcileCreatesThePod(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod created")
	}
	if pod.Labels[podspec.LabelGroup] != "lobby" {
		t.Errorf("pod labels = %v, want the group label", pod.Labels)
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != "Server" {
		t.Errorf("owner references = %+v, want a Server controller ref", pod.OwnerReferences)
	}

	srv := f.server("lobby-x7k2")
	if srv.Status.PodName != "lobby-x7k2" {
		t.Errorf("status.podName = %q, want lobby-x7k2", srv.Status.PodName)
	}
	if srv.Status.Phase != string(phase.Pending) {
		t.Errorf("phase = %q, want Pending until the pod runs", srv.Status.Phase)
	}
	if srv.Status.StartedAt == nil {
		t.Error("status.startedAt not set; the startup deadline needs it")
	}
	if !containsString(srv.Finalizers, ServerFinalizer) {
		t.Errorf("finalizers = %v, want %s", srv.Finalizers, ServerFinalizer)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")
	first, _ := f.pod("lobby-x7k2")

	f.reconcile("lobby-x7k2")
	second, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("pod disappeared on the second reconcile")
	}
	if first.UID != second.UID {
		t.Error("second reconcile replaced the pod")
	}
}

func TestReadyGateNeedsBothSignals(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	pod, _ := f.pod("lobby-x7k2")
	uid := string(pod.UID)

	// Only the probe is green.
	f.setPodRunning("lobby-x7k2", true)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q with a green probe alone, want Starting", got)
	}
	if len(f.registrar.registered) != 0 {
		t.Errorf("registered = %v, want no registration before the agent is ready", f.registrar.registered)
	}

	// Now the agent as well.
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Ready) {
		t.Fatalf("phase = %q with both signals, want Ready", srv.Status.Phase)
	}
	if !srv.Status.Registered {
		t.Error("status.registered = false, want true")
	}
	if srv.Status.Address != "10.42.3.17:25565" {
		t.Errorf("status.address = %q, want 10.42.3.17:25565", srv.Status.Address)
	}
	if len(f.registrar.registered) != 1 {
		t.Errorf("registered = %v, want exactly one registration", f.registrar.registered)
	}
}

func TestReadinessLossDeregistersImmediately(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")

	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Starting) {
		t.Errorf("phase = %q after readiness loss, want Starting", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("status.registered = true after readiness loss, want false")
	}
	if srv.Status.ReadinessLosses != 1 {
		t.Errorf("readinessLosses = %d, want 1", srv.Status.ReadinessLosses)
	}
	if len(f.registrar.deregistered) != 1 {
		t.Errorf("deregistered = %v, want exactly one deregistration", f.registrar.deregistered)
	}
}

func TestStreamLossDeregistersAfterTheGracePeriod(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")

	f.agents.Disconnect(uid)
	f.clock.Advance(phase.StreamDownGrace - time.Second)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q inside the grace period, want Ready", got)
	}

	f.clock.Advance(2 * time.Second)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Errorf("phase = %q past the grace period, want Starting", got)
	}
}

func TestOccupiedLabelTracksThePlayerCount(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")

	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	pod, _ := f.pod("lobby-x7k2")
	if pod.Labels[podspec.LabelOccupied] != "true" {
		t.Errorf("occupied label = %q with 3 players, want true", pod.Labels[podspec.LabelOccupied])
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	pod, _ = f.pod("lobby-x7k2")
	if _, ok := pod.Labels[podspec.LabelOccupied]; ok {
		t.Errorf("occupied label still set with 0 players: %v", pod.Labels)
	}
}

func TestStalePlayerCountKeepsThePodOccupied(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	f.clock.Advance(11 * time.Second) // past twice the 5s report interval
	f.reconcile("lobby-x7k2")

	pod, _ := f.pod("lobby-x7k2")
	if pod.Labels[podspec.LabelOccupied] != "true" {
		t.Errorf("occupied label = %q on a stale count, want true — stale means occupied",
			pod.Labels[podspec.LabelOccupied])
	}
}

func TestDeletionDrainsBeforeThePodIsDeleted(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}

	// First reconcile after deletion: drain starts, pod survives.
	f.reconcile("lobby-x7k2")
	srv = f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Draining) {
		t.Fatalf("phase = %q, want Draining", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("a draining server must not stay registered")
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want one drain command", f.registrar.drained)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}

	// Players keep it alive.
	f.clock.Advance(time.Second)
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}

	// Now the server runs empty.
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod still there after the drain finished")
	}

	// The finalizer goes once the pod is gone.
	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Server still present after the drain: %v", err)
	}
}

// TestDeletionAfterAReadinessLossStillDrains covers the case the phase alone
// cannot describe: the server reached Ready, was registered, then lost a ready
// signal and fell back to Starting — deregistered, but with its players still
// connected, because deregistering only stops new joins. Deleting it now must
// drain, not terminate.
func TestDeletionAfterAReadinessLossStillDrains(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Starting) {
		t.Fatalf("phase = %q after the readiness loss, want Starting", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("status.registered = true after the readiness loss, want false")
	}
	if !srv.Status.WasRegistered {
		t.Fatal("status.wasRegistered = false, want true — the server was registered once")
	}

	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv = f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Draining) {
		t.Errorf("phase = %q, want Draining — a once-registered server still holds its players",
			srv.Status.Phase)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want one drain command", f.registrar.drained)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}
}

func TestLostPodTerminatesTheServer(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")

	pod, _ := f.pod("lobby-x7k2")
	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Terminating) {
		t.Errorf("phase = %q after the pod vanished, want Terminating", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("a server whose pod is gone must be deregistered")
	}
}

func TestDrainTimeoutTerminatesLoudly(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if err := f.c.Delete(f.ctx, f.server("lobby-x7k2")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")

	// Keep reporting players so the drain can never finish on its own.
	for i := 0; i < 13; i++ {
		f.clock.Advance(5 * time.Second)
		if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
	}

	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod survived the drain timeout")
	}
}

// TestCrashLoopingOnlyLooksAtTheMinecraftContainer pins the scope of the
// crash-loop check: PodTerminal aborts a running drain, so a crash-looping
// sidecar must never be able to cut short the drain of a healthy server.
func TestCrashLoopingOnlyLooksAtTheMinecraftContainer(t *testing.T) {
	backoff := corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}

	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "metrics-sidecar",
			RestartCount: MaxContainerRestarts + 5,
			State:        backoff,
		}},
	}}
	if crashLooping(pod) {
		t.Error("a crash-looping sidecar counted as terminal; only the Minecraft container may")
	}

	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:         podspec.ContainerName,
		RestartCount: MaxContainerRestarts,
		State:        backoff,
	})
	if !crashLooping(pod) {
		t.Error("a crash-looping Minecraft container was not detected")
	}
}
