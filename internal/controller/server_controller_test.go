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
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// bringUpReady creates a fresh server and walks it all the way into phase
// Ready, returning the pod UID the agent registry is keyed on.
func bringUpReady(t *testing.T, f *fixture, name string) string {
	t.Helper()
	f.createServer(name)
	return bringUpNamed(t, f, name)
}

// driveToFailed flaps the server past MaxReadinessLosses so it ends up in phase
// Failed with its players still connected, and returns once it is there.
func driveToFailed(t *testing.T, f *fixture, name string) {
	t.Helper()
	for i := int32(0); i < phase.MaxReadinessLosses; i++ {
		f.setPodRunning(name, false)
		f.reconcile(name)
		if i < phase.MaxReadinessLosses-1 {
			f.setPodRunning(name, true)
			f.reconcile(name)
		}
	}
	f.reconcile(name)
	if got := f.server(name).Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q after %d readiness losses, want Failed", got, phase.MaxReadinessLosses)
	}
}

// setPodFailed fakes the other thing a kubelet does: the process is down for
// good and the pod will not run again. The readiness condition is left exactly
// as it was, because a kubelet does not tidy it up either — the occupied label
// must not depend on that.
func (f *fixture) setPodFailed(name string) {
	f.t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		f.t.Fatalf("pod %s not found", name)
	}
	pod.Status.Phase = corev1.PodFailed
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		f.t.Fatalf("update pod status: %v", err)
	}
}

// setPodCrashLooping fakes a pod whose Minecraft container cannot stay up.
// The pod phase stays Running, which is what separates this case from
// PodFailed: the API server waves an eviction of a Failed or Succeeded pod
// through without consulting any PodDisruptionBudget, so only a crash-looping
// pod can show what the budget actually does to a drain.
func (f *fixture) setPodCrashLooping(name string) {
	f.t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		f.t.Fatalf("pod %s not found", name)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(f.clock.Now()),
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         podspec.ContainerName,
		RestartCount: MaxContainerRestarts,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
	}}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		f.t.Fatalf("update pod status: %v", err)
	}
}

// pods lists every pod in the fixture namespace that still exists.
func (f *fixture) pods() []corev1.Pod {
	f.t.Helper()
	list := &corev1.PodList{}
	if err := f.c.List(f.ctx, list, client.InNamespace(f.ns)); err != nil {
		f.t.Fatalf("list pods: %v", err)
	}
	return list.Items
}

// TestServerFailedStraightFromReadyClearsReadySince pins the cross-file
// invariant CountFailures states and depends on: "A Failed server carries no
// ReadySince (the Server controller clears it on the way out of Ready), so a
// corpse can never look like the success that ends its own streak"
// (backoff.go). Break the clearing and a group's failure streak resets on its
// own corpse, so the count never climbs past 1 for a server that was Ready and
// then died — and the whole per-group backoff stops bounding anything.
//
// It has to go through the terminal-pod transition rather than the flapping
// one. Every other fixture in this package reaches Failed via Starting, and
// entering Starting clears readySince on its own, so a test built that way
// passes with `case phase.Failed: srv.Status.ReadySince = nil` deleted — which
// is exactly what the whole-branch review measured. phase.Decide's PodTerminal
// branch goes Ready -> Failed in one step and touches no other clearing, so
// only the Failed arm of the status switch can be what empties the field here.
//
// Its counting-side companion is
// TestCountFailuresTakesASuccessFromAnyPhaseAndWhyThatIsSafe in
// backoff_test.go, which shows what a corpse that kept its readySince does to
// a streak.
func TestServerFailedStraightFromReadyClearsReadySince(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")
	if f.server("lobby-x7k2").Status.ReadySince == nil {
		t.Fatal("status.readySince was not stamped at Ready; the clearing below would prove nothing")
	}

	// The kubelet's other verdict: the process is down for good.
	f.setPodFailed("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if got := srv.Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q after the pod reached a terminal phase, want Failed", got)
	}
	if srv.Status.ReadinessLosses != 0 {
		t.Fatalf("readinessLosses = %d, want 0: this server must reach Failed without going "+
			"through Starting, or the clearing under test is not the one being observed",
			srv.Status.ReadinessLosses)
	}
	if srv.Status.ReadySince != nil {
		t.Errorf("status.readySince = %v on a Failed server, want nil: a corpse that keeps its "+
			"readySince looks to CountFailures like the success that ends its own streak",
			srv.Status.ReadySince.UTC())
	}
}

// TestLongLivedReadyServerSurvivesAReadinessBlip is the regression test for the
// stale startup deadline. status.startedAt is written once at pod creation and
// never refreshed, so StartupDeadlineReached is true for every server older than
// the deadline. Before the fix, one probe blip on a server that had been serving
// for hours put it in Starting and the very next reconcile failed it — and the
// Failed retention then deleted its pod with everyone still on board.
func TestLongLivedReadyServerSurvivesAReadinessBlip(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 40, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	// Far past the 5 minute startup deadline.
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 40, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q after two hours of healthy service, want Ready", got)
	}

	// One probe blip.
	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")
	blipped := f.server("lobby-x7k2")
	if got := blipped.Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q after the blip, want Starting", got)
	}
	// The mechanism that makes this safe: entering Starting re-arms the
	// startup deadline, so the recovery attempt gets a full window.
	if blipped.Status.StartedAt == nil || !blipped.Status.StartedAt.Time.Equal(f.clock.Now()) {
		var got any
		if blipped.Status.StartedAt != nil {
			got = blipped.Status.StartedAt.UTC()
		}
		t.Fatalf("status.startedAt = %v, want it re-armed to %v on entry into Starting",
			got, f.clock.Now().UTC())
	}

	// The reconcile that used to fail it: still Starting, still past the
	// startup deadline, but this server was playable once.
	f.reconcile("lobby-x7k2")
	srv := f.server("lobby-x7k2")
	if srv.Status.Phase == string(phase.Failed) {
		t.Fatal("a long-lived server was failed by its stale startup deadline")
	}
	if srv.Status.Phase != string(phase.Starting) {
		t.Fatalf("phase = %q, want Starting", srv.Status.Phase)
	}

	// And it recovers.
	f.setPodRunning("lobby-x7k2", true)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase = %q after recovery, want Ready", got)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while 40 players were online — core invariant broken")
	}
}

// TestServerThatNeverBecomesPlayableFailsAtTheDeadline is the other side of the
// re-armed startup deadline: a server that was never playable must still be
// failed when the deadline passes, exactly as before.
func TestServerThatNeverBecomesPlayableFailsAtTheDeadline(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	// The pod runs but its probe never turns green.
	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q, want Starting", got)
	}

	f.clock.Advance(6 * time.Minute) // the fixture's deadline is 5 minutes
	f.reconcile("lobby-x7k2")

	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Failed) {
		t.Errorf("phase = %q past the startup deadline, want Failed", got)
	}
	if len(f.registrar.drained) != 0 {
		t.Errorf("drained = %v, want none — this server never took a player", f.registrar.drained)
	}
}

// TestServerThatCannotRecoverIsFailedAndDrained is the zombie the first attempt
// at the startup-deadline fix created. Exempting a once-registered server from
// the deadline meant a server that fell out of Ready with a permanently red
// probe was never failed at all: the flap counter cannot catch it either,
// because losses are only counted on a Ready -> Starting transition that a
// permanently red probe never produces again. Its players sat on a server that
// fails its own health check, forever. Re-arming the clock instead of exempting
// the server fails it one deadline after the fall-back, and drains it.
func TestServerThatCannotRecoverIsFailedAndDrained(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	// The probe goes red and never comes back.
	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q after the fall-back, want Starting", got)
	}

	f.clock.Advance(200 * time.Minute)
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Failed) {
		t.Fatalf("phase = %q after 200 minutes with a red probe, want Failed — the server is a zombie",
			srv.Status.Phase)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want exactly one drain — 9 players were left on a dead server",
			f.registrar.drained)
	}
	if srv.Status.DrainStartedAt == nil {
		t.Error("status.drainStartedAt not set, so the drain would never time out")
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted with 9 players still on it — core invariant broken")
	}
}

// TestZombieIsCaughtUnderAContinuousReconcileLoop drives the reconciler the way
// the operator actually runs it — once per resync interval — instead of jumping
// the clock and reconciling once. That difference is the whole point: the
// startup deadline is re-armed only on *entry* into Starting, and a re-arm on
// every pass would push the deadline out forever under a real loop while
// leaving a single-reconcile test perfectly green. The zombie would be back,
// with its players still on board.
func TestZombieIsCaughtUnderAContinuousReconcileLoop(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	// The probe goes red and never comes back.
	f.setPodRunning("lobby-x7k2", false)

	const ticks = 200 // 200 * 5s resync is far past the 5 minute deadline
	failedAfter := -1
	for i := 0; i < ticks; i++ {
		f.clock.Advance(resyncInterval)
		if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
		if f.server("lobby-x7k2").Status.Phase == string(phase.Failed) {
			failedAfter = i
			break
		}
	}

	srv := f.server("lobby-x7k2")
	if failedAfter < 0 {
		t.Fatalf("still %q with 9 players after %d reconciles over %v — the startup deadline never fired",
			srv.Status.Phase, ticks, time.Duration(ticks)*resyncInterval)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want exactly one drain — 9 players were left on a dead server",
			f.registrar.drained)
	}
	if srv.Status.DrainStartedAt == nil {
		t.Error("status.drainStartedAt not set, so the drain would never time out")
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted with 9 players still on it — core invariant broken")
	}
}

// TestFailedServerDrainsBeforeItsPodIsDeleted covers the second half of the
// same hole: a server can reach Failed with its sessions untouched, and the
// retention path must not delete that pod without moving the players off first.
func TestFailedServerDrainsBeforeItsPodIsDeleted(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	driveToFailed(t, f, "lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want a drain when the server failed with players on it", f.registrar.drained)
	}
	if srv.Status.DrainStartedAt == nil {
		t.Error("status.drainStartedAt not set on a failed server that is being drained")
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod of a failed server deleted while players were online")
	}

	// While the drain runs the pod is kept, the drain clock is not pushed out by
	// the repeated decisions, and the command is not re-broadcast on every pass
	// — the Failed branch returns StartDrain each time, but the real registrar
	// fans out to every proxy.
	drainStarted := srv.Status.DrainStartedAt.DeepCopy()
	for i := 0; i < 3; i++ {
		f.clock.Advance(10 * time.Second)
		if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
		if _, ok := f.pod("lobby-x7k2"); !ok {
			t.Fatalf("pod deleted while 6 players were still online (tick %d)", i)
		}
	}
	if got := f.server("lobby-x7k2").Status.DrainStartedAt; !got.Equal(drainStarted) {
		t.Errorf("drainStartedAt rewritten: %v, want the original %v", got, drainStarted)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v after four reconciles of a draining failed server, want exactly one broadcast",
			f.registrar.drained)
	}

	// Once it runs empty and the retention has passed, it goes.
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("pod of an empty failed server survived its retention")
	}
}

// TestFailedServerIsCleanedUpOnceItsDrainDeadlinePasses documents the escape
// hatch deliberately: the drain of a failed server is bounded, so one stuck
// player cannot pin a broken server forever. The group's drain timeout is 60s
// and its failed retention an hour, so by the time the retention elapses the
// drain deadline has always passed — this is the intended end of that path,
// not an accident.
func TestFailedServerIsCleanedUpOnceItsDrainDeadlinePasses(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	driveToFailed(t, f, "lobby-x7k2")

	// Past both the drain deadline and the retention, with players still on.
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("a failed server pinned by players survived its drain deadline")
	}
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Terminating) {
		t.Errorf("phase = %q, want Terminating", got)
	}
}

// TestDeletingAFailedServerDrainsThenReleasesIt pins that a Failed server
// honours a deletion request: it drains first and releases its finalizer after.
func TestDeletingAFailedServerDrainsThenReleasesIt(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	driveToFailed(t, f, "lobby-x7k2")

	if err := f.c.Delete(f.ctx, f.server("lobby-x7k2")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("deleting a failed server dropped its players")
	}
	if len(f.registrar.drained) == 0 {
		t.Error("deleting a failed server issued no drain")
	}

	// This is the window where the Failed branch really does return StartDrain
	// on every single pass: occupied, once registered, deletion pending, drain
	// deadline not yet reached. The clock stays put so the state holds. The
	// command must still go out exactly once — the real registrar broadcasts to
	// every proxy, and this loop would otherwise be eleven fan-outs.
	for i := 0; i < 10; i++ {
		f.reconcile("lobby-x7k2")
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v after ten further reconciles of a deleted, occupied failed server, want exactly one broadcast",
			f.registrar.drained)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were still online — core invariant broken")
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod still there after the failed server was emptied")
	}

	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer not released on a deleted failed server: %v", err)
	}
}

// TestServerOutlivingItsGroupStillDrainsAndReleasesItself pins that a missing
// ServerGroup does not freeze the controller. Before the fix both the group and
// the network lookup returned before the finalizer, the drain and the label
// sync, so such a Server kept its pod and its finalizer forever and the orphan
// sweep of Task 11 would deadlock on it.
func TestServerOutlivingItsGroupStillDrainsAndReleasesItself(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if err := f.c.Delete(f.ctx, f.group); err != nil {
		t.Fatalf("delete ServerGroup: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	accepted := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonGroupNotFound {
		t.Errorf("Accepted condition = %+v, want False/GroupNotFound", accepted)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod dropped when the group disappeared")
	}

	// It must still drain and still let go.
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Draining) {
		t.Errorf("phase = %q for a groupless server being deleted, want Draining", got)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod leaked: still there after the drain finished")
	}

	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer never released on a groupless server: %v", err)
	}
}

// TestAPersistentServerDrainsAndReleasesItself pins that a group's type bounds
// nothing about its servers' lifecycle. It was written while the Server
// controller refused to build a pod for a persistent group, to prove that the
// refusal was not an early return skipping the finalizer, the drain and the
// release — a Server that could never be deleted is the same deadlock as one
// whose group is gone. The refusal is gone as of this milestone; the ordering
// it was deliberately placed after is not, and neither is what that ordering
// protects, so the walk stays and the pod now comes from the controller
// itself rather than being stood in for.
func TestAPersistentServerDrainsAndReleasesItself(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)
	f.createPersistentServer(t, "survival", 0)

	f.reconcile("survival-0")
	srv := f.server("survival-0")
	pod, ok := f.pod("survival-0")
	if !ok {
		t.Fatal("no pod was built for the persistent server")
	}
	if !containsString(srv.Finalizers, ServerFinalizer) {
		t.Fatalf("finalizers = %v, want %s", srv.Finalizers, ServerFinalizer)
	}
	accepted := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionTrue ||
		accepted.Reason != spawneryv1alpha1.ReasonAccepted {
		t.Errorf("Accepted condition = %+v, want True/%s", accepted, spawneryv1alpha1.ReasonAccepted)
	}

	uid := string(pod.UID)
	f.setPodRunning("survival-0", true)
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	if err := f.agents.ReportPlayers(uid, 5, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("survival-0") // Pending -> Starting
	f.reconcile("survival-0") // Starting -> Ready
	if got := f.server("survival-0").Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q, want Ready", got)
	}

	// The point of the whole test: it must still drain and still let go.
	if err := f.c.Delete(f.ctx, f.server("survival-0")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("survival-0")
	if got := f.server("survival-0").Status.Phase; got != string(phase.Draining) {
		t.Errorf("phase = %q for a deleted persistent server, want Draining", got)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want one drain command", f.registrar.drained)
	}
	if _, ok := f.pod("survival-0"); !ok {
		t.Fatal("pod deleted while 5 players were online — core invariant broken")
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("survival-0")
	if _, ok := f.pod("survival-0"); ok {
		t.Fatal("pod leaked: still there after the drain finished")
	}

	f.reconcile("survival-0")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "survival-0", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer never released on a persistent-group server: %v", err)
	}

	// And the world is still there once everything that referenced it has
	// gone. TestDeletingAPersistentServerLeavesItsClaim asks the same question
	// one reconcile after the deletion; this asks it after the object itself
	// has been released, which is the state a recreated ordinal actually
	// arrives in.
	if f.claim("survival-0-data") == nil {
		t.Error("the claim went with the server it outlived; the world is gone")
	}
}

// TestPodIsAdoptedAfterALostStatusWrite covers a crash between Create(pod) and
// the status update. fetchPod falls back to the server name and finds the pod,
// so without adoption the creation branch is skipped forever while podName and
// startedAt stay empty — the startup deadline could never fire and PodLost
// could never be detected.
func TestPodIsAdoptedAfterALostStatusWrite(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod created")
	}
	originalUID := pod.UID

	// Simulate the lost write: the pod exists, the status does not know it.
	srv := f.server("lobby-x7k2")
	srv.Status.PodName = ""
	srv.Status.StartedAt = nil
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("clear status: %v", err)
	}

	f.reconcile("lobby-x7k2")

	srv = f.server("lobby-x7k2")
	if srv.Status.PodName != "lobby-x7k2" {
		t.Errorf("status.podName = %q, want the existing pod adopted", srv.Status.PodName)
	}
	if srv.Status.StartedAt == nil {
		t.Error("status.startedAt not restored from the pod; the startup deadline needs it")
	}
	if got := f.pods(); len(got) != 1 {
		t.Errorf("%d pods in the namespace, want exactly 1 — a second pod was created", len(got))
	}
	if again, ok := f.pod("lobby-x7k2"); !ok || again.UID != originalUID {
		t.Error("the original pod was replaced instead of adopted")
	}
}

// TestForeignPodWithTheSameNameIsNotAdopted is the other half of adoption: the
// owner reference has to be verified, or a Server would take charge of a
// workload it never created.
func TestForeignPodWithTheSameNameIsNotAdopted(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")

	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-x7k2",
			Namespace: f.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       f.group.Name,
				UID:        f.group.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "not-ours", Image: "busybox"}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create foreign pod: %v", err)
	}

	f.reconcile("lobby-x7k2")

	if got := f.server("lobby-x7k2").Status.PodName; got != "" {
		t.Errorf("status.podName = %q, want empty — a foreign pod was adopted", got)
	}
	got, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("the foreign pod was deleted")
	}
	if got.UID != foreign.UID {
		t.Error("the foreign pod was replaced")
	}
	if _, labelled := got.Labels[podspec.LabelOccupied]; labelled {
		t.Error("the controller labelled a pod it does not own")
	}
	if len(f.pods()) != 1 {
		t.Errorf("%d pods, want 1 — the controller created a second pod over the conflict", len(f.pods()))
	}
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

// TestPodThatCrashedWithPlayersOnItLosesTheOccupiedLabel is the regression test
// for the half of the terminal-pod exemption that was missing. The exemption
// only ever sat inside the stale disjunct, and snap.Players > 0 is evaluated
// first and wins — so it only helped a pod whose last reported count was zero,
// which is exactly the case the original test happened to build.
//
// Nothing tells the agent registry to forget a pod, so a server that crashed
// with seven players on it goes on reporting seven for as long as the Server
// object is retained. The dead pod therefore kept spawnery.cloud/occupied=true,
// the group's PodDisruptionBudget kept minAvailable at 1 with currentHealthy at
// 0, and kubectl drain on that node never finished — for the whole failed
// retention, an hour by default.
//
// The count is deliberately re-reported fresh on every pass here: staleness is
// not what this is about. A terminal pod has no sessions, whatever the last
// count said, because they went down with the process.
func TestPodThatCrashedWithPlayersOnItLosesTheOccupiedLabel(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod for lobby-x7k2")
	}
	if pod.Labels[podspec.LabelOccupied] != "true" {
		t.Fatalf("occupied label = %q on a live server with 7 players, want true",
			pod.Labels[podspec.LabelOccupied])
	}

	// The pod dies with all seven still on it.
	f.setPodFailed("lobby-x7k2")
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q after the pod reached PodFailed, want Failed", got)
	}

	// It is retained for diagnosis, and for that whole window the label must
	// stay off however often the registry repeats its last count.
	for i := 0; i < 60; i++ {
		if err := f.agents.ReportPlayers(uid, 7, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")

		p, ok := f.pod("lobby-x7k2")
		if !ok {
			t.Fatalf("pass %d: the retained pod disappeared before its retention elapsed", i)
		}
		if v, set := p.Labels[podspec.LabelOccupied]; set {
			t.Fatalf("pass %d: the pod of a server that crashed with 7 players carries %s=%q; "+
				"the eviction API will refuse to release it for the whole retention window",
				i, podspec.LabelOccupied, v)
		}
		f.clock.Advance(resyncInterval)
	}
}

// TestOccupiedLabelNeedsTheServerToHaveBeenRegistered pins the half of the
// label rule that had no test of its own. A count we cannot trust hides players
// only on a server the proxies actually route to; a server that never got that
// far has nobody on it, unreadable count or not.
//
// Without this, dropping status.wasRegistered from the rule left the whole
// suite green on the label side — every server that failed to come up would
// have been labelled occupied and pinned the group's budget, and the mirror
// check in ServerView.Occupied was the only thing catching the same mistake.
func TestOccupiedLabelNeedsTheServerToHaveBeenRegistered(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	// The pod runs but the probe never turns green and no agent ever connects,
	// so the registry knows nothing about it: unknown means stale.
	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Starting) {
		t.Fatalf("phase = %q, want Starting", srv.Status.Phase)
	}
	if srv.Status.WasRegistered {
		t.Fatal("status.wasRegistered = true on a server that never reached Ready")
	}

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod for lobby-x7k2")
	}
	uid := string(pod.UID)
	if !f.agents.Lookup(uid).PlayersStale {
		t.Fatal("the count of a server with no agent must read as stale, or this test proves nothing")
	}

	for i := 0; i < 3; i++ {
		f.clock.Advance(resyncInterval)
		f.reconcile("lobby-x7k2")
		p, ok := f.pod("lobby-x7k2")
		if !ok {
			t.Fatalf("pass %d: pod disappeared", i)
		}
		if v, set := p.Labels[podspec.LabelOccupied]; set {
			t.Fatalf("pass %d: a server that was never registered carries %s=%q; "+
				"nobody was ever routed to it, and its group can now never shrink", i, podspec.LabelOccupied, v)
		}
	}
}

// TestFinalizerIsWrittenBeforeTheFirstStatusWrite pins the ordering that
// ensureFinalizer exists to express. Update returns the persisted object, and
// because status is a subresource the API server does not take the status from
// us — controller-runtime writes the persisted, on a first reconcile empty,
// status back over the object in memory. Any condition set before that call is
// silently dropped, and the Status().Update at the end of applyDecision then
// persists a Server that never had it.
//
// A Server whose group does not exist is the cheapest case that writes a
// condition on its very first reconcile, which is also the only reconcile on
// which the finalizer is added. Move a status write ahead of ensureFinalizer
// and the Accepted condition below disappears.
func TestFinalizerIsWrittenBeforeTheFirstStatusWrite(t *testing.T) {
	f := newFixture(t)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-1", Namespace: f.ns},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "no-such-group"}},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create Server: %v", err)
	}

	f.reconcile("ghost-1")

	got := f.server("ghost-1")
	if !containsString(got.Finalizers, ServerFinalizer) {
		t.Fatalf("finalizers = %v, want %s on the first reconcile", got.Finalizers, ServerFinalizer)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil {
		t.Fatalf("no Accepted condition after the first reconcile, conditions = %+v — "+
			"a status write ran before the finalizer Update and was overwritten by the persisted object",
			got.Status.Conditions)
	}
	if accepted.Status != metav1.ConditionFalse || accepted.Reason != spawneryv1alpha1.ReasonGroupNotFound {
		t.Errorf("Accepted condition = %+v, want False/%s", accepted, spawneryv1alpha1.ReasonGroupNotFound)
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

// denyingCreator refuses every create the way an admission webhook, a
// ResourceQuota on ConfigMaps or a policy in a customer namespace refuses the
// bootstrap's writes: permanently, and not because of anything the operator
// could wait out.
type denyingCreator struct {
	client.Client
}

func (denyingCreator) Create(context.Context, client.Object, ...client.CreateOption) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "configmaps"},
		podspec.CAConfigMapName,
		errors.New("denied by an admission policy"))
}

// A namespace the operator is not allowed to bootstrap is not a passing phase.
// The waiting-for-the-first-certificate case clears itself within seconds, but
// a refused write does not, and nothing else in this reconciler would ever say
// so: status.startedAt is only written once a pod exists, so
// StartupDeadlineReached can never fire and the Server can never fail out.
// Without a condition and an event, every Server in such a namespace sits in
// Pending forever with a log line as its only trace.
func TestReconcileReportsANamespaceItCannotBootstrap(t *testing.T) {
	f := newFixture(t)

	// newFixture's own Network reconcile already bootstrapped this namespace
	// (that is this task's whole point), so the CA ConfigMap and both
	// ServiceAccounts are already here. Remove them, or the denying
	// Bootstrapper below has nothing left to deny: CreateOrUpdate finds the
	// ConfigMap present and unchanged and never calls Create, and
	// ensureServiceAccounts's Get-then-Create finds each ServiceAccount
	// already there and never calls Create either.
	if err := f.c.Delete(f.ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.CAConfigMapName, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("delete the fixture's CA ConfigMap: %v", err)
	}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		if err := f.c.Delete(f.ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		}); err != nil {
			t.Fatalf("delete the fixture's %s ServiceAccount: %v", name, err)
		}
	}

	rec := newRecorder()
	f.reconc.Recorder = rec
	f.reconc.Bootstrap = &Bootstrapper{
		Client: denyingCreator{Client: f.c},
		Reader: f.c,
		CA:     func() []byte { return []byte("test-ca") },
	}

	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("a pod was created although the namespace could not be bootstrapped")
	}
	got := f.server("lobby-x7k2")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, ReasonNamespaceNotBootstrapped) {
		t.Errorf("conditions = %+v, want Accepted=False with reason %s — a refusal nobody "+
			"can see on the CR is a Server stuck in Pending with no explanation",
			got.Status.Conditions, ReasonNamespaceNotBootstrapped)
	}

	recorded := drainEvents(rec)
	found := false
	for _, ev := range recorded {
		if strings.Contains(ev, ReasonNamespaceNotBootstrapped) {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %q, want one naming %s", recorded, ReasonNamespaceNotBootstrapped)
	}

	// The condition is a report, not a verdict: once the obstacle is gone the
	// next resync creates the pod, with no restart and no manual step.
	f.reconc.Bootstrap = &Bootstrapper{
		Client: f.c, Reader: f.c,
		CA: func() []byte { return []byte("test-ca") },
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("no pod was created after the namespace could be bootstrapped again")
	}
	if !hasCondition(f.server("lobby-x7k2").Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Error("the Accepted condition stayed false after the namespace was bootstrapped")
	}
}

// While the registrar was a no-op this window was harmless. With a real one it
// is not: if the status write is lost while players are already joining, a
// deletion in that window takes the branch "never registered, terminate
// immediately, no drain" and throws them out instead of moving them.
//
// The assertion is deliberately made from inside Register, against the API
// server rather than against the in-memory object: what matters is that the
// intent was durable before the proxies heard anything, and only a read-back
// can tell the difference.
//
// The read-back also carries the Accepted condition, set earlier in this same
// reconcile. That is not the regression guard against downgrading this
// mid-reconcile write to a non-status Update (see ensureFinalizer) — by the
// time d.Register runs here, Accepted has already been durable for two
// reconciles courtesy of bringUpReady, so these assertions would pass
// whichever kind of Update wrote it; wasDurable above is what actually catches
// that regression. What the assertion here, and the one on the Accepted
// condition after the server settles below, catch independently is a status
// struct replaced wholesale inside a legitimate status call.
func TestWasRegisteredIsDurableBeforeTheProxiesAreTold(t *testing.T) {
	f := newFixture(t)

	var wasDurable bool
	f.registrar.onRegister = func(s *spawneryv1alpha1.Server) error {
		persisted := &spawneryv1alpha1.Server{}
		key := types.NamespacedName{Name: s.Name, Namespace: f.ns}
		if err := f.c.Get(f.ctx, key, persisted); err != nil {
			t.Errorf("read the Server back inside Register: %v", err)
			return nil
		}
		wasDurable = persisted.Status.WasRegistered
		if !hasCondition(persisted.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
			metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
			t.Error("the Accepted condition set earlier this reconcile did not survive the mid-reconcile status write")
		}
		return nil
	}

	bringUpReady(t, f, "lobby-order")

	if len(f.registrar.registered) != 1 {
		t.Fatalf("registered = %v, want exactly one registration", f.registrar.registered)
	}
	if !wasDurable {
		t.Error("the proxies were told about a server whose registration intent was not yet persisted")
	}
	if !hasCondition(f.server("lobby-order").Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Error("the Accepted condition is not True once the server reached Ready")
	}
}

// status.registered must not be left true with no registration behind it
// looking like a success: the reconcile has to fail so the next pass tries
// again. status.wasRegistered is the opposite case: it is deliberately left
// true even though this registration failed, because it is the fail-safe
// that forces a later deletion to drain rather than terminate outright, and a
// failed attempt does not undo the fact that the proxies may already have
// been told about an earlier one.
func TestAFailedRegisterFailsTheReconcile(t *testing.T) {
	f := newFixture(t)
	f.registrar.onRegister = func(*spawneryv1alpha1.Server) error {
		return errors.New("no proxy accepted the registration")
	}

	f.createServer("lobby-refuse")
	f.reconcile("lobby-refuse")
	pod, ok := f.pod("lobby-refuse")
	if !ok {
		t.Fatal("reconcile did not create the pod")
	}
	f.setPodRunning("lobby-refuse", false)
	f.reconcile("lobby-refuse")

	f.setPodRunning("lobby-refuse", true)
	f.agents.Connect(string(pod.UID), agentRoleServer())
	f.agents.MarkReady(string(pod.UID))
	if err := f.agents.ReportPlayers(string(pod.UID), 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	// f.reconcile fails the test on error, so call the reconciler directly.
	_, err := f.reconc.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "lobby-refuse", Namespace: f.ns},
	})
	if err == nil {
		t.Fatal("a refused registration did not fail the reconcile")
	}

	got := f.server("lobby-refuse").Status
	if got.Registered {
		t.Error("status.registered is true after a registration that failed")
	}
	if !got.WasRegistered {
		t.Error("status.wasRegistered is false, but it must be written before Register is even called")
	}
}

// TestServerRetireFieldsRoundTripThroughTheAPIServer exists to catch the
// specific mistake of editing the Go type and forgetting make manifests: the
// API server silently drops unknown fields, so a Go-only change round-trips
// as zero. Only a round trip through a real API server catches that, which is
// why this is here and not a struct test.
func TestServerRetireFieldsRoundTripThroughTheAPIServer(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "retire-roundtrip", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			Retire:   true,
		},
	}
	if err := f.c.Create(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = f.c.Delete(ctx, srv) })

	now := metav1.Now()
	srv.Status.RetiringSince = &now
	if err := f.c.Status().Update(ctx, srv); err != nil {
		t.Fatalf("status update: %v", err)
	}

	got := &spawneryv1alpha1.Server{}
	if err := f.c.Get(ctx, client.ObjectKeyFromObject(srv), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Spec.Retire {
		t.Error("spec.retire did not survive the API server; run make manifests")
	}
	if got.Status.RetiringSince == nil {
		t.Error("status.retiringSince did not survive the API server; run make manifests")
	}
}

// TestRetiringServerStampsItsClockAndDeregisters proves the server side of
// Task 7's ask: a Server patched with spec.retire moves to phase Retiring,
// stamps status.retiringSince so maxStaleSeconds has something to measure
// from, and comes off the proxies so it stops taking joins — without moving
// the players already on it.
func TestRetiringServerStampsItsClockAndDeregisters(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "a") // registered with the proxies

	srv := f.server("a")
	srv.Spec.Retire = true // what the group controller does
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcile("a")

	got := f.server("a")
	if got.Status.Phase != string(phase.Retiring) {
		t.Errorf("phase = %q, want Retiring", got.Status.Phase)
	}
	if got.Status.RetiringSince == nil {
		t.Error("retiringSince was not stamped, so maxStaleSeconds can never fire")
	}
	if got.Status.Registered {
		t.Error("a retiring server is still registered, so it is still taking joins")
	}
}

// retiringFor builds a Server that has been sitting in phase Retiring for d,
// for the collectInputs unit tests below.
func retiringFor(d time.Duration) *spawneryv1alpha1.Server {
	since := metav1.NewTime(time.Now().Add(-d))
	return &spawneryv1alpha1.Server{
		Status: spawneryv1alpha1.ServerStatus{
			Phase:         string(phase.Retiring),
			RetiringSince: &since,
		},
	}
}

// groupWithMaxStale builds a ServerGroup configured with the given
// maxStaleSeconds, for the collectInputs unit tests below.
func groupWithMaxStale(seconds int32) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Update: &spawneryv1alpha1.UpdateSpec{MaxStaleSeconds: seconds},
		},
	}
}

// collectInputsReconciler is the minimal ServerReconciler collectInputs
// needs: a Clock and an Agents registry, since collectInputs looks up the
// agent snapshot unconditionally. It carries no client, so it only works
// against Server/ServerGroup values built in memory, not ones read from
// envtest.
func collectInputsReconciler(clock func() time.Time) *ServerReconciler {
	return &ServerReconciler{
		Clock:  clock,
		Agents: agent.New(clock, 5*time.Second, clock()),
	}
}

func TestMaxStaleZeroNeverForcesADrain(t *testing.T) {
	// The default. A retiring server with players waits indefinitely, which
	// is the promise the whole feature makes.
	in := collectInputsReconciler(time.Now).
		collectInputs(retiringFor(time.Hour*24), groupWithMaxStale(0), nil, false, false)
	if in.MaxStaleReached {
		t.Error("MaxStaleReached with maxStaleSeconds = 0")
	}
}

func TestMaxStaleFiresOnceTheWaitExceedsIt(t *testing.T) {
	in := collectInputsReconciler(time.Now).
		collectInputs(retiringFor(2*time.Minute), groupWithMaxStale(60), nil, false, false)
	if !in.MaxStaleReached {
		t.Error("MaxStaleReached is false after twice the configured wait")
	}
}

// TestRetiringSinceSurvivesRepeatedReconciles is the wiring counterpart to
// TestRetiringServerStampsItsClockAndDeregisters, which only reconciles once.
// status.retiringSince is stamped with the same guarded-once pattern as
// DrainStartedAt and FailedAt (see TestFailedServerDrainsBeforeItsPodIsDeleted),
// and that guard is exactly what maxStaleSeconds depends on: if it were
// re-stamped on every pass, the deadline measured from it would never arrive.
// This drives a real server through several reconciles with the clock moving
// between them and pins the timestamp as identical throughout, not merely
// non-nil.
func TestRetiringSinceSurvivesRepeatedReconciles(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	srv.Spec.Retire = true
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcile("lobby-x7k2")

	got := f.server("lobby-x7k2")
	if got.Status.Phase != string(phase.Retiring) {
		t.Fatalf("phase = %q after retire, want Retiring", got.Status.Phase)
	}
	if got.Status.RetiringSince == nil {
		t.Fatal("retiringSince not stamped on entry to Retiring")
	}
	retiringSince := got.Status.RetiringSince.DeepCopy()

	// The group's default Update is unset (MaxStaleSeconds 0, "never"), so
	// nothing here should push the server out of Retiring — the point is
	// purely whether the timestamp itself holds still.
	for i := 0; i < 3; i++ {
		f.clock.Advance(10 * time.Second)
		if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
	}

	got = f.server("lobby-x7k2")
	if got.Status.Phase != string(phase.Retiring) {
		t.Errorf("phase = %q after further reconciles, want still Retiring", got.Status.Phase)
	}
	if rs := got.Status.RetiringSince; !rs.Equal(retiringSince) {
		t.Errorf("retiringSince rewritten: %v, want the original %v", rs, retiringSince)
	}
}

// TestMaxStaleSecondsEscalatesARetiringServerToDraining drives a Server
// through the real machine — collectInputs, phase.Decide and applyDecision
// together, not collectInputs called directly as
// TestMaxStaleFiresOnceTheWaitExceedsIt does — to confirm a configured
// maxStaleSeconds actually forces a stale Retiring server into Draining, with
// the players still on it: the escalation must go through the drain path
// (phase.Retiring's MaxStaleReached branch sets StartDrain), never straight to
// Terminating.
func TestMaxStaleSecondsEscalatesARetiringServerToDraining(t *testing.T) {
	f := newFixture(t)
	f.group.Spec.Update = &spawneryv1alpha1.UpdateSpec{MaxStaleSeconds: 30}
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update ServerGroup: %v", err)
	}

	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	srv.Spec.Retire = true
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Retiring) {
		t.Fatalf("phase = %q after retire, want Retiring", got)
	}

	// Advance past the 30s window in several passes, the same shape as
	// TestDrainTimeoutTerminatesLoudly's thirteen reconciles past a deadline.
	for i := 0; i < 4; i++ {
		f.clock.Advance(10 * time.Second)
		if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
	}

	got := f.server("lobby-x7k2")
	if got.Status.Phase != string(phase.Draining) {
		t.Errorf("phase = %q after the stale window elapsed, want Draining", got.Status.Phase)
	}
	if got.Status.DrainStartedAt == nil {
		t.Error("drainStartedAt not set; the maxStaleSeconds escalation must go through the drain path")
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want exactly one drain broadcast", f.registrar.drained)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while 6 players were still online — core invariant broken")
	}
}

// TestMaxStaleZeroLeavesARetiringServerAloneIndefinitely is the complement of
// TestMaxStaleSecondsEscalatesARetiringServerToDraining: the same clock
// advance, but with maxStaleSeconds at 0 ("never"), must never push the
// server out of Retiring. This is the case that actually protects players —
// a group that never configured a stale window must not have one inferred
// for it — so it gets its own fixture and its own server rather than reusing
// the one that proved the positive case, which would leave the zero path
// resting on inference instead of its own assertion.
func TestMaxStaleZeroLeavesARetiringServerAloneIndefinitely(t *testing.T) {
	f := newFixture(t)
	// f.group's default Update is unset, i.e. MaxStaleSeconds 0 — made
	// explicit here so the zero case doesn't quietly depend on the fixture's
	// default staying zero.
	f.group.Spec.Update = &spawneryv1alpha1.UpdateSpec{MaxStaleSeconds: 0}
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update ServerGroup: %v", err)
	}

	uid := bringUpReady(t, f, "lobby-zero")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-zero")

	srv := f.server("lobby-zero")
	srv.Spec.Retire = true
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcile("lobby-zero")
	if got := f.server("lobby-zero").Status.Phase; got != string(phase.Retiring) {
		t.Fatalf("phase = %q after retire, want Retiring", got)
	}

	// Same clock advance as the escalating case above.
	for i := 0; i < 4; i++ {
		f.clock.Advance(10 * time.Second)
		if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-zero")
	}

	got := f.server("lobby-zero")
	if got.Status.Phase != string(phase.Retiring) {
		t.Errorf("phase = %q after the same clock advance with maxStaleSeconds = 0, want still Retiring", got.Status.Phase)
	}
	if got.Status.DrainStartedAt != nil {
		t.Error("drainStartedAt set with maxStaleSeconds = 0; a server with no configured stale window must wait forever")
	}
	if len(f.registrar.drained) != 0 {
		t.Errorf("drained = %v, want no drain broadcast with maxStaleSeconds = 0", f.registrar.drained)
	}
	if _, ok := f.pod("lobby-zero"); !ok {
		t.Fatal("pod deleted while players were still online — core invariant broken")
	}
}

// createPersistentServer adds the Server holding one ordinal of a persistent
// group, the way ServerGroupReconciler.createPersistentServer builds it: the
// derived name, spec.ordinal filled in, the three labels newServer stamps, and
// the group as its *controller* rather than merely an owner. It lets a test
// drive the Server controller alone, without the group controller in the
// picture.
//
// Controller: true is the part that would be easy to leave off and is not
// decoration. metav1.IsControlledBy is what the Server controller's adoption
// path asks about a pod, and a fixture that builds ownership a second way is a
// fixture that can make a controller look right about a shape production never
// hands it.
func (f *fixture) createPersistentServer(t *testing.T, group string, ordinal int32) *spawneryv1alpha1.Server {
	t.Helper()
	owner := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: group, Namespace: f.ns}, owner); err != nil {
		t.Fatalf("get ServerGroup %s: %v", group, err)
	}
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PersistentServerName(group, ordinal),
			Namespace: f.ns,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   owner.Spec.NetworkRef.Name,
				podspec.LabelGroup:     owner.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spawneryv1alpha1.GroupVersion.String(),
				Kind:               "ServerGroup",
				Name:               owner.Name,
				UID:                owner.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: owner.Name},
			GroupGeneration: owner.Generation,
			Ordinal:         &ordinal,
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create Server %s: %v", srv.Name, err)
	}
	return srv
}

// claim reads a PersistentVolumeClaim by name and returns nil when there is
// none, rather than failing the test the way f.server does. Absence is exactly
// what the assertions below have to be able to ask about.
//
// A claim carrying a deletion timestamp counts as gone, the same rule f.pod
// applies to a pod and for a sharper reason: the API server's
// StorageObjectInUseProtection admission plugin puts a pvc-protection
// finalizer on every claim at creation, and no controller runs in envtest to
// take it off again. A deleted claim therefore stays readable for the rest of
// the test, and without this rule "the claim is still here" would be true of a
// world somebody had just asked to be destroyed — verified by mutation: a
// Delete added to the Server controller's deletion path is invisible to
// TestDeletingAPersistentServerLeavesItsClaim without it.
func (f *fixture) claim(name string) *corev1.PersistentVolumeClaim {
	f.t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, pvc); err != nil {
		return nil
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return nil
	}
	return pvc
}

// TestAPersistentServerGetsItsClaimBeforeItsPod pins the order master design
// 6.1 asks for, and the retention that makes a world survive.
//
// envtest runs no provisioner, so the claim never reaches Bound and the pod
// never runs. No test in this file asserts either — they assert the objects,
// which is all this layer can honestly show.
//
// The order in the name is the weakest of the three, and saying so here is
// cheaper than a reader finding out: both objects exist by the time a
// reconcile returns, so what these assertions actually catch is a claim that
// is never created at all, or a pod that is not. Swapping the two Creates
// round was mutation-tested and passes this test unchanged.
//
// Not pinned is not the same as unpinnable, and the difference is worth being
// precise about. A client.Client decorator recording the order of Create calls
// would pin it — recordingRegistrar.onRegister in suite_test.go is this
// fixture's existing instance of that technique, for an ordering that likewise
// "cannot be seen from outside the call". It is left undone because the
// consequence of a swap is soft rather than because the tool is missing: both
// objects land in the same reconcile, the API server does not check that a
// pod's claim exists, and a pod whose claim is not there yet is merely
// unschedulable — the scheduler retries it, and the claim arrives
// microseconds later. Add the decorator on the day something between the two
// Creates can return early with the pod already made; today nothing between
// them can.
func TestAPersistentServerGetsItsClaimBeforeItsPod(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)
	srv := f.createPersistentServer(t, "survival", 0)

	f.reconcile(srv.Name)

	claim := f.claim("survival-0-data")
	if claim == nil {
		t.Fatal("no claim was created; a persistent pod would reference one that does not exist and stay Pending forever")
	}
	if len(claim.OwnerReferences) != 0 {
		t.Error("the claim carries an owner reference; deleting the server would take the world with it")
	}
	if _, ok := f.pod(srv.Name); !ok {
		t.Error("no pod was created")
	}
}

// retirePodTheWayAKubeletWould removes a pod object that is only carrying a
// deletion timestamp, which is the last step of a termination and the one step
// envtest cannot take on its own: with no kubelet, a pod that was bound to a
// node keeps its timestamp and answers Get for the rest of the test. A force
// delete does what the kubelet's confirmation would.
func (f *fixture) retirePodTheWayAKubeletWould(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if err := f.c.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("force delete pod %s: %v", pod.Name, err)
	}
}

// recreateOrdinalOverATerminatingPod builds the state finding 1 of the
// whole-branch review is about: a persistent ordinal whose Server object has
// been replaced while the pod of the previous one is still terminating.
//
// It is a persistent group's state in practice: the name is derived from the
// ordinal and reused across every generation of the Server object, so the
// group's replacement meets its predecessor's pod under the identical name.
//
// The pod is bound to a node before it is deleted, and that step is the
// mechanism rather than scenery: the API server force-deletes an *unscheduled*
// pod outright, because no kubelet owes it a confirmation. Once a pod is bound
// it waits for one, and envtest runs no kubelet to send it — so the deleted pod
// keeps its deletion timestamp for the rest of the test, which is what a node
// gone NotReady looks like on a real cluster.
//
// It returns the terminating pod so a test can finish the termination.
func recreateOrdinalOverATerminatingPod(t *testing.T, f *fixture) *corev1.Pod {
	t.Helper()
	f.createPersistentGroup(t, "survival", 1)
	first := f.createPersistentServer(t, "survival", 0)
	f.reconcile(first.Name)

	pod, ok := f.pod("survival-0")
	if !ok {
		t.Fatal("no pod for the first server, so there is no name for the second one to collide with")
	}
	f.bindPodToNode(t, pod, f.ensureNode(t, "node-holding-"+f.ns, false).Name)
	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	if _, stillLive := f.pod(pod.Name); stillLive {
		t.Fatal("the deleted pod is not terminating, so nothing holds its name")
	}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: pod.Name, Namespace: f.ns}, &corev1.Pod{}); err != nil {
		t.Fatalf("the deleted pod left the API server outright, so the collision this test is about "+
			"cannot happen: %v", err)
	}
	if err := f.c.Delete(f.ctx, f.server(first.Name)); err != nil {
		t.Fatalf("delete the first server: %v", err)
	}
	f.reconcile(first.Name)
	if _, present := f.serverIfPresent(first.Name); present {
		t.Fatal("the first server kept its finalizer, so the ordinal was never free to be rebuilt")
	}

	f.createPersistentServer(t, "survival", 0)
	return pod
}

// TestARecreatedOrdinalWaitsForItsPredecessorsPod pins the first step of a
// cycle nothing inside this operator bounds. What ends it from outside is the
// pod finishing termination — a grace period ordinarily, and never, on a node
// that has gone away.
//
// Creating into a name the API server still holds gets AlreadyExists on the
// pod, which the create block tolerates — so the controller would record
// status.podName and emit PodCreated for a pod it did not create, and the next
// pass would read that pod as lost and delete this Server. The group rebuilds
// the ordinal, and round it goes at the five-second resync: nothing reaches
// Failed, so consecutiveFailures never moves and neither BackingOff nor
// Degraded ever fires. A fresh Server with an empty status.podName cannot raise
// PodLost, which is why waiting is the whole fix.
func TestARecreatedOrdinalWaitsForItsPredecessorsPod(t *testing.T) {
	f := newFixture(t)
	recreateOrdinalOverATerminatingPod(t, f)

	f.reconcile("survival-0")

	got := f.server("survival-0")
	if got.Status.PodName != "" {
		t.Errorf("status.podName = %q while the predecessor's pod is still terminating; "+
			"the controller claimed a pod it did not create, and the next pass reads it as lost",
			got.Status.PodName)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Reason != ReasonPodNameTerminating {
		t.Errorf("Accepted = %+v, want reason %s: an ordinal waiting on its predecessor's pod "+
			"is indistinguishable from a slow start otherwise", accepted, ReasonPodNameTerminating)
	}
}

// describePods lists every pod in the fixture's namespace with the two fields
// that decide this test, for a failure message rather than for an assertion.
func describePods(f *fixture) string {
	pods := &corev1.PodList{}
	if err := f.c.List(f.ctx, pods, client.InNamespace(f.ns)); err != nil {
		return "could not list: " + err.Error()
	}
	if len(pods.Items) == 0 {
		return "none"
	}
	out := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		deleting := "live"
		if !pods.Items[i].DeletionTimestamp.IsZero() {
			deleting = "terminating since " + pods.Items[i].DeletionTimestamp.String()
		}
		out = append(out, fmt.Sprintf("%s (%s, node %q)",
			pods.Items[i].Name, deleting, pods.Items[i].Spec.NodeName))
	}
	return strings.Join(out, "; ")
}

// podUnderNameIsStill answers whether the pod holding a name is still the one
// with this UID.
//
// The UID and not the name, because in this test they are the same name: the
// ordinal's pod name is reused across generations, so "is a pod called
// survival-0 present" is true both when the predecessor is lingering and when
// the successor has been created — the two states the failure has to tell
// apart. A first draft of this asked by name and reported "predecessor still
// present: true" about a healthy successor, which would have sent the next
// reader in exactly the wrong direction.
func podUnderNameIsStill(f *fixture, name string, uid types.UID) bool {
	pod := &corev1.Pod{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: name}, pod); err != nil {
		return false
	}
	return pod.UID == uid
}

// TestARecreatedOrdinalCreatesItsPodOnceThePredecessorIsGone is the other half:
// the wait ends by itself, and ends with a pod this controller really did
// create. Without it the fix above would be indistinguishable from a Server
// that never gets a pod at all.
//
// # It flaked once, on 2026-08-22, and has never done it again
//
// It failed during milestone 6d's Task 6 `make test`, passed in isolation and
// on a full rerun, and 6d changed nothing it touches. Nothing was captured.
// Recorded here rather than left to memory, because an unrecorded flake is
// rediagnosed from scratch by whoever meets the second one.
//
// One thing is ruled out rather than assumed: **it is not cache lag.**
// internal/testenv's Client is client.New, a direct client with no informer
// behind it, so the hypothesis everyone reaches for first with envtest cannot
// be the mechanism. Sixty runs in isolation and five full-package runs on
// 2026-08-23 did not reproduce it, and ci.yml's `test` job -- go test -race
// ./... on every push and pull request -- had concluded failure not once
// across 86 runs as of 2026-08-25. So this is one occurrence standing against
// roughly ninety executions.
//
// The diagnostic above is what a second occurrence gets that the first did
// not: the Accepted condition, every pod with its deletionTimestamp and node,
// and whether the pod under the name is still the predecessor by UID.
func TestARecreatedOrdinalCreatesItsPodOnceThePredecessorIsGone(t *testing.T) {
	f := newFixture(t)
	terminating := recreateOrdinalOverATerminatingPod(t, f)
	f.reconcile("survival-0")

	f.retirePodTheWayAKubeletWould(t, terminating)
	f.reconcile("survival-0")

	got := f.server("survival-0")
	if got.Status.PodName != "survival-0" {
		// This is the one assertion in this file that has been seen to fail
		// without explanation: docs/known-issues.md records it flaking once
		// during milestone 6d, passing in isolation and on a full rerun, and
		// nothing was captured. Sixty runs in isolation and five full-package
		// runs on 2026-08-23 did not reproduce it either.
		//
		// So the failure prints what it saw. Whatever the mechanism is, the
		// three facts below separate the candidates: a lingering predecessor
		// says the force delete did not take, a Server carrying
		// PodNameTerminating with no such pod present says the controller
		// decided against a pod that is no longer there, and an empty
		// namespace with a clean condition says something else entirely
		// refused the create. A second occurrence should be a diagnosis, not
		// another data point.
		t.Fatalf("status.podName = %q once the predecessor's pod is gone, want survival-0\n"+
			"  Accepted condition: %+v\n"+
			"  pods in the namespace: %s\n"+
			"  the pod under that name is still the predecessor: %t",
			got.Status.PodName,
			meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted),
			describePods(f), podUnderNameIsStill(f, terminating.Name, terminating.UID))
	}
	if _, ok := f.pod("survival-0"); !ok {
		t.Error("no pod once the name was free")
	}
	// The wait clears rather than sticking: the switch at the top of Reconcile
	// sets Accepted back to True on every pass that resolves the group and the
	// network, and this is what says the block above did not park a reason on
	// the object that outlives what it described.
	accepted := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v once the pod was created, want True: the wait has to clear", accepted)
	}
}

// TestAnExistingClaimIsLeftExactlyAsItIs pins design §3.3's second property,
// "created, never updated", which until this test was pinned by nothing: no
// test put a claim in the way that disagreed with spec.storage, so replacing
// the claim Create in server_controller.go with the controllerutil.CreateOrUpdate
// this same package uses six times over left the suite green.
//
// It is what stands between 5b and a world that has been resized or
// reconfigured underneath the group. A claim already carrying a size, a class
// or an access mode other than the one spec.storage asks for is the ordinary
// case rather than a corruption — a world grown by hand, or created under an
// earlier spec — and 5a's answer is to leave it alone and let 5b decide what
// growth means. An update would at best be rejected by the API server (a PVC's
// storageClassName and accessModes are immutable, and its size may not shrink)
// and at worst succeed.
//
// resourceVersion is the assertion that makes "byte-identical" mean it: any
// write at all moves it, including one that lands the same values, and a
// CreateOrUpdate that rewrote only the labels was mutation-tested and fails
// exactly this line and no other.
//
// The three field assertions under it cannot fail while that one holds, and
// saying so is cheaper than a reader assuming otherwise: the API server
// forbids changing the spec of an existing claim, bar a bound claim's resource
// request and its volumeAttributesClassName, and envtest binds nothing — so a
// write that reached these
// fields would be rejected and fail the reconcile before any assertion ran —
// which is what the full CreateOrUpdate mutation did. They are here to say
// what byte-identical is protecting, in the terms 5b will change.
func TestAnExistingClaimIsLeftExactlyAsItIs(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)

	// Disagreeing with spec.storage (10Gi, no class, ReadWriteOnce by the CRD
	// default) on each of the three fields BuildDataClaim renders.
	existing := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.DataClaimName("survival-0"),
			Namespace: f.ns,
			Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To("chosen-when-the-world-was-made"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")},
			},
		},
	}
	if err := f.c.Create(f.ctx, existing); err != nil {
		t.Fatalf("create the claim already holding the world: %v", err)
	}
	before := f.claim("survival-0-data")
	if before == nil {
		t.Fatal("no claim to begin with")
	}

	f.createPersistentServer(t, "survival", 0)
	f.reconcile("survival-0")

	after := f.claim("survival-0-data")
	if after == nil {
		t.Fatal("the claim is gone")
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("the claim was written to (resourceVersion %s -> %s): size %v, class %s, modes %v",
			before.ResourceVersion, after.ResourceVersion,
			after.Spec.Resources.Requests[corev1.ResourceStorage],
			ptr.Deref(after.Spec.StorageClassName, "<unset>"), after.Spec.AccessModes)
	}
	if got := after.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("50Gi")) != 0 {
		t.Errorf("storage request = %v, want the 50Gi the claim already had", got)
	}
	if after.Spec.StorageClassName == nil || *after.Spec.StorageClassName != "chosen-when-the-world-was-made" {
		t.Errorf("storageClassName = %v, want the class the volume was provisioned from", after.Spec.StorageClassName)
	}
	if len(after.Spec.AccessModes) != 1 || after.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("accessModes = %v, want the modes the claim already had", after.Spec.AccessModes)
	}
}

// TestGrowingStorageSizePatchesTheClaim pins 5b's one write to an object 5a
// declared created-never-written: growing spec.storage.size reaches the
// claim, and only that one field.
//
// The reconcile that has to notice the growth runs against a server whose
// pod already exists, which is the ordinary case for a persistent server
// that has been up for a while — createPod is false by then, so nothing in
// the pod-creation path would ever see the new size. growClaim runs on every
// pass for exactly that reason.
//
// envtest runs no CSI driver and no external-resizer, and its API server
// enforces the same resize admission a real cluster does: a claim's
// spec.resources.requests is only mutable once the claim is Bound, and only
// against a StorageClass with allowVolumeExpansion. Both are faked here the
// same way setPodRunning fakes the kubelet elsewhere in this file — the
// StorageClass is real (creating one needs no controller behind it), but
// nothing ever binds the claim to a volume, so its Bound status is written
// by hand rather than earned. Confirmed by hand before this test was kept:
// without the fake bind, r.Patch in growClaim fails with envtest's real
// "only dynamically provisioned pvc can be resized" error, the same as it
// would against a claim nobody's storage class ever bound.
func TestGrowingStorageSizePatchesTheClaim(t *testing.T) {
	f := newFixture(t)
	class := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "expandable-" + f.ns},
		Provisioner: "kubernetes.io/no-provisioner",
	}
	class.AllowVolumeExpansion = ptr.To(true)
	if err := f.c.Create(f.ctx, class); err != nil {
		t.Fatalf("create StorageClass: %v", err)
	}

	// storage.storageClassName is immutable once set (CEL rule on
	// ServerGroupSpec), so it has to be there at creation -- built by hand
	// rather than through createPersistentGroup for that one reason.
	replicas := int32(1)
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: f.network.Name},
			Type:                          spawneryv1alpha1.ServerGroupPersistent,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    100,
			Replicas:                      &replicas,
			TerminationGracePeriodSeconds: 60,
			FailedRetentionSeconds:        3600,
			Drain:                         &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 60},
			Storage: &spawneryv1alpha1.StorageSpec{
				Size:             resource.MustParse("10Gi"),
				StorageClassName: &class.Name,
			},
		},
	}
	if err := f.c.Create(f.ctx, group); err != nil {
		t.Fatalf("create persistent ServerGroup: %v", err)
	}
	f.createPersistentServer(t, "survival", 0)
	f.reconcile("survival-0")

	before := f.claim("survival-0-data")
	if before == nil {
		t.Fatal("no claim to grow")
	} else if size := before.Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("claim requests %s before growth, want the fixture's 10Gi", size.String())
	}
	before.Status.Phase = corev1.ClaimBound
	before.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
	if err := f.c.Status().Update(f.ctx, before); err != nil {
		t.Fatalf("fake-bind the claim: %v", err)
	}

	group.Spec.Storage.Size = resource.MustParse("20Gi")
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("grow the group: %v", err)
	}
	f.reconcile("survival-0")

	claim := f.claim("survival-0-data")
	if claim == nil {
		t.Fatal("the claim is gone")
	}
	got := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("claim requests %s, group asks for 20Gi", got.String())
	}
}

// TestAResizePendingClaimMarksItsServer pins the reaction, not the trigger.
//
// envtest runs no CSI driver and no external-resizer, so nothing in this
// suite can make FileSystemResizePending appear on its own — the condition
// is written onto the claim's status by hand, the same way growClaim's test
// fake-binds a claim envtest's real provisioners never would. That is the
// honest way to test this: the operator's job is to read the condition and
// mirror it, not to produce it. What this test does NOT prove is that a real
// CSI driver ever sets the condition, or that restarting the pod actually
// makes the filesystem follow — both are outside this operator's reach and
// outside envtest's.
func TestAResizePendingClaimMarksItsServer(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)
	f.createPersistentServer(t, "survival", 0)
	f.reconcile("survival-0")

	claim := f.claim("survival-0-data")
	if claim == nil {
		t.Fatal("no claim to mark")
	}
	claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
		Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
		Status: corev1.ConditionTrue,
	}}
	if err := f.c.Status().Update(f.ctx, claim); err != nil {
		t.Fatalf("set the condition: %v", err)
	}

	f.reconcile("survival-0")

	if srv := f.server("survival-0"); !srv.Status.StorageResizePending {
		t.Fatal("the claim asked for a restart and the server does not say so")
	}
}

// TestAClaimLargerThanTheSpecIsLeftAlone is a regression guard, not a new
// property: it already passed before growClaim existed, because 5a never
// wrote to a claim at all, and it has to go on passing now that growClaim
// does. A claim someone grew by hand is not the operator's to shrink —
// want.Cmp(have) <= 0 in growClaim is what leaves it alone — and the CRD's
// own shrink guard on spec.storage.size means the API would refuse the
// correction anyway, even if growClaim tried.
func TestAClaimLargerThanTheSpecIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1) // 10Gi, per the fixture.

	hand := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.DataClaimName("survival-0"),
			Namespace: f.ns,
			Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")},
			},
		},
	}
	if err := f.c.Create(f.ctx, hand); err != nil {
		t.Fatalf("create the hand-grown claim: %v", err)
	}
	before := f.claim("survival-0-data")
	if before == nil {
		t.Fatal("no claim to begin with")
	}

	f.createPersistentServer(t, "survival", 0)
	f.reconcile("survival-0")

	after := f.claim("survival-0-data")
	if after == nil {
		t.Fatal("the claim is gone")
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("the claim was written to (resourceVersion %s -> %s); a hand-grown claim is not a divergence to heal",
			before.ResourceVersion, after.ResourceVersion)
	}
	if got := after.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("50Gi")) != 0 {
		t.Errorf("claim requests %s; want the 50Gi it already had", got.String())
	}
	// Left alone means no patch was attempted, not that one was attempted and
	// refused. Narrowing growClaim's guard from want.Cmp(have) <= 0 to == 0
	// makes it patch this claim downward; the API refuses that, the claim is
	// unchanged, and both assertions above still hold -- so this is what tells
	// the two apart.
	if srv := f.server("survival-0"); srv.Status.StorageResizeError != "" {
		t.Errorf("storageResizeError = %q, want empty; nothing should have been sent to this claim",
			srv.Status.StorageResizeError)
	}
}

// TestARecreatedOrdinalMountsTheClaimItLeft is the half of design §6's own
// headline test that was never written. That section names the one envtest
// case that matters as "deleting a Server leaves its claim standing, **and the
// same ordinal picks it up again**"; only the first clause had a test.
//
// Writing this is what would have surfaced the terminating-pod collision the
// two tests above are about, which is why the recreation here goes through the
// same helper rather than around it: the ordinal cannot pick anything up until
// its predecessor's pod has finished.
//
// What "picks it up again" has to mean at this layer is that the claim that
// was there is still there, and is the one the new pod mounts. A claim deleted
// and remade under the same name shows up here as
// an *absent* claim rather than a changed one, and that is worth stating: the
// pvc-protection finalizer envtest never clears means a deleted claim cannot
// leave, so the remake gets AlreadyExists and f.claim — which counts a
// deletion timestamp as gone — reports nothing. The "no claim" assertion is
// therefore the one that catches a stray Delete on this path, and it was
// mutation-tested against exactly that: a Delete added to the finalizer-release
// branch, which TestDeletingAPersistentServerLeavesItsClaim did not reach
// before this commit.
func TestARecreatedOrdinalMountsTheClaimItLeft(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)
	first := f.createPersistentServer(t, "survival", 0)
	f.reconcile(first.Name)
	if f.claim("survival-0-data") == nil {
		t.Fatal("no claim to begin with")
	}

	pod, ok := f.pod("survival-0")
	if !ok {
		t.Fatal("no pod for the first server")
	}
	f.bindPodToNode(t, pod, f.ensureNode(t, "node-holding-"+f.ns, false).Name)
	if err := f.c.Delete(f.ctx, f.server(first.Name)); err != nil {
		t.Fatalf("delete the first server: %v", err)
	}
	f.reconcile(first.Name)
	f.retirePodTheWayAKubeletWould(t, pod)
	f.reconcile(first.Name)
	if _, present := f.serverIfPresent(first.Name); present {
		t.Fatal("the first server kept its finalizer, so the ordinal was never free to be rebuilt")
	}

	f.createPersistentServer(t, "survival", 0)
	f.reconcile("survival-0")

	after := f.claim("survival-0-data")
	if after == nil {
		t.Fatal("the recreated ordinal has no claim")
	}
	newPod, ok := f.pod("survival-0")
	if !ok {
		t.Fatal("the recreated ordinal has no pod")
	}
	if got := claimsMounted(newPod); !slices.Contains(got, "survival-0-data") {
		t.Errorf("the new pod mounts claims %v, want survival-0-data among them: the ordinal came "+
			"back onto a different volume, so its world is still on the one it left", got)
	}
}

// claimsMounted is the claim names a pod's volumes are backed by. It reads the
// pod rather than trusting BuildServerPod, because the question here is what
// the ordinal actually came back onto.
func claimsMounted(pod *corev1.Pod) []string {
	var names []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			names = append(names, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return names
}

// TestDeletingAPersistentServerLeavesItsClaim is the property the whole
// milestone turns on.
//
// What it catches is a Delete on the claim from the Server controller's
// deletion path. It cannot catch an owner reference: envtest runs no garbage
// collector, so an owned claim survives its owner here exactly as an unowned
// one does — which is why the ownerReferences assertion sits in
// TestAPersistentServerGetsItsClaimBeforeItsPod and is checked on the object
// rather than through a deletion.
//
// It takes two reconciles, and the second is not a belt-and-braces repeat. The
// first sees the pod still there and only asks for its deletion; the branch
// that releases the finalizer and lets the object go needs the pod already
// gone, so it runs on the second. A Delete added to that branch was invisible
// to the single-reconcile version of this test — mutation-tested — while the
// object it destroyed was the world.
func TestDeletingAPersistentServerLeavesItsClaim(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)
	srv := f.createPersistentServer(t, "survival", 0)
	f.reconcile(srv.Name)
	if f.claim("survival-0-data") == nil {
		t.Fatal("no claim to begin with")
	}

	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcile(srv.Name)
	if _, present := f.serverIfPresent(srv.Name); present {
		t.Fatal("the server is still here, so the deletion path this test is about never finished")
	}

	if f.claim("survival-0-data") == nil {
		t.Fatal("the claim went with the server; the world is gone")
	}
}

// refusingPodCreator refuses to create pods and creates everything else
// normally. The reconcile under test also writes a CA ConfigMap, two
// ServiceAccounts and — for a persistent group — a PVC, and a client that
// refused all of those would stop the pass long before it reached the pod.
type refusingPodCreator struct {
	client.Client
	err error
}

func (r refusingPodCreator) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return r.err
	}
	return r.Client.Create(ctx, obj, opts...)
}

// A pod the API server refuses is the shape milestone 4's last unmet
// precondition described: a Server that never got a pod, sitting in Pending
// and occupying its group's slot.
//
// Returning the error alone left nothing on the object. status.podName stays
// empty, so the pod-lost path never applies; status.startedAt is only written
// beside a pod that exists, so StartupDeadlineReached can never fire; and the
// Server therefore sat there for as long as the refusal stood with a log line
// as its only trace — the same silence TestReconcileReportsANamespaceItCannot
// Bootstrap closed for the branch one call earlier.
func TestReconcileReportsAPodTheAPIServerRefused(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	f.reconc.Recorder = rec
	f.reconc.Client = refusingPodCreator{
		Client: f.c,
		err: apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "lobby-r4t9",
			errors.New(`violates PodSecurity "restricted:latest"`)),
	}

	f.createServer("lobby-r4t9")
	// Fatals if the reconcile returns an error, which is half the point: a
	// refusal the namespace's policy causes is not something to retry with
	// backoff and no report.
	f.reconcile("lobby-r4t9")

	if _, ok := f.pod("lobby-r4t9"); ok {
		t.Fatal("a pod exists although the client refused to create one")
	}
	got := f.server("lobby-r4t9")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, ReasonServerPodRejected) {
		t.Errorf("conditions = %+v, want Accepted=False with reason %s — a Server whose pod "+
			"the API server refuses has nothing else that would ever say so",
			got.Status.Conditions, ReasonServerPodRejected)
	}
	if got.Status.PodName != "" {
		t.Errorf("status.podName = %q after a refused create, want it empty", got.Status.PodName)
	}

	recorded := drainEvents(rec)
	found := false
	for _, ev := range recorded {
		if strings.Contains(ev, ReasonServerPodRejected) {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %q, want one naming %s", recorded, ReasonServerPodRejected)
	}

	// A report, not a verdict: the resync requeue recovers on its own once the
	// namespace's policy stops refusing.
	f.reconc.Client = f.c
	f.reconcile("lobby-r4t9")
	if _, ok := f.pod("lobby-r4t9"); !ok {
		t.Fatal("no pod after the refusal was lifted; the condition became a dead end")
	}
	if got := f.server("lobby-r4t9"); !meta.IsStatusConditionTrue(
		got.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		t.Errorf("Accepted = %+v once the pod was created, want True",
			meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted))
	}
}

// fallbackGroup is what a Server runs on when its own group is gone, and it
// used to stamp Ephemeral on every one of them — milestone 5's last open
// precondition. A unit test rather than an envtest because the type is what is
// under test, and the one branch it decides in Reconcile (!IsEphemeral) reaches
// only calls that are no-ops without a storage spec: driving it through the API
// server would prove the no-ops, not the type.
func TestTheFallbackGroupTakesItsTypeFromTheOrdinal(t *testing.T) {
	ephemeral := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}
	if got := fallbackGroup(ephemeral); !got.IsEphemeral() {
		t.Errorf("a Server with no spec.ordinal falls back to %q, want Ephemeral", got.Spec.Type)
	}

	persistent := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "survival"},
			Ordinal:  ptr.To[int32](0),
		},
	}
	if got := fallbackGroup(persistent); got.IsEphemeral() {
		t.Errorf("a Server carrying spec.ordinal falls back to %q, want Persistent. "+
			"createPersistentServer is the only thing that sets that field, so it is "+
			"what identifies the type of a group that is no longer there to ask",
			got.Spec.Type)
	}

	// Ordinal 0 is the one that matters: a value check rather than a nil check
	// would read the first ordinal of every persistent group as ephemeral, and
	// ordinal 0 is the one that exists in every persistent group there is.
	if got := fallbackGroup(persistent); *persistent.Spec.Ordinal != 0 || got.IsEphemeral() {
		t.Errorf("ordinal 0 read as ephemeral: %q", got.Spec.Type)
	}

	// The timings do not move with the type, which is what the precondition
	// this closes actually claimed they did.
	if a, b := fallbackGroup(ephemeral), fallbackGroup(persistent); a.DrainTimeout() != b.DrainTimeout() ||
		a.FailedRetention() != b.FailedRetention() || a.UpdateMaxStale() != b.UpdateMaxStale() {
		t.Errorf("the fallback timings differ by type: ephemeral %v/%v/%v, persistent %v/%v/%v",
			a.DrainTimeout(), a.FailedRetention(), a.UpdateMaxStale(),
			b.DrainTimeout(), b.FailedRetention(), b.UpdateMaxStale())
	}
}

// The other half of a refused pod: after the report comes the reclaim.
//
// A Server whose pod the API server refuses has an empty status.podName, so
// PodLost never applies, and until 2026-08-24 status.startedAt was written
// only beside a pod — so it had no clock at all and sat in Pending occupying
// its group's slot for as long as the refusal stood. It now fails at a
// deadline derived from the group: drain.timeoutSeconds plus the startup
// deadline.
//
// The middle step is the one worth having. Between the startup deadline and
// the creation deadline the server must stay Pending and must not be reported
// as "did not become ready in time": it never had a pod, so it never failed to
// become ready, and the remedy is at whatever refused the create.
func TestAServerWhosePodIsNeverCreatedFailsAtItsOwnDeadline(t *testing.T) {
	f := newFixture(t)
	f.reconc.Recorder = newRecorder()
	f.reconc.Client = refusingPodCreator{
		Client: f.c,
		err: apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "lobby-q8n4",
			errors.New(`violates PodSecurity "restricted:latest"`)),
	}

	f.createServer("lobby-q8n4")
	f.reconcile("lobby-q8n4")

	srv := f.server("lobby-q8n4")
	if srv.Status.StartedAt == nil {
		t.Fatal("status.startedAt is unset after a pass that accepted the Server; without it " +
			"nothing can ever put a clock on a pod that is never created")
	}
	if srv.Status.PodName != "" {
		t.Fatalf("status.podName = %q, want empty", srv.Status.PodName)
	}

	// Past the startup deadline, short of the creation deadline.
	f.clock.Advance(f.reconc.StartupDeadline + time.Second)
	f.reconcile("lobby-q8n4")
	srv = f.server("lobby-q8n4")
	if got := srv.Status.Phase; got != string(phase.Pending) {
		t.Errorf("phase = %q past the startup deadline, want %q. A server that never had a "+
			"pod did not fail to become ready — the startup deadline is not its question",
			got, phase.Pending)
	}
	if c := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionReady); c != nil &&
		c.Reason == phase.ReasonStartupTimeout {
		t.Errorf("Ready = %+v, want any reason but %s", c, phase.ReasonStartupTimeout)
	}

	// Past the creation deadline: drain.timeoutSeconds plus the startup
	// deadline, measured from the pass that accepted the Server.
	f.clock.Advance(f.group.DrainTimeout() + time.Second)
	f.reconcile("lobby-q8n4")
	srv = f.server("lobby-q8n4")
	if got := srv.Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q past the creation deadline, want %q — nothing else would ever "+
			"have failed this server, so its slot was held for good", got, phase.Failed)
	}
	if c := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionReady); c == nil ||
		c.Reason != phase.ReasonPodNeverCreated {
		t.Errorf("Ready = %+v, want reason %s", c, phase.ReasonPodNeverCreated)
	}
}

// The creation deadline must not run while the pod's *name* is held by another
// pod, and this is the case that made it matter.
//
// An ordinal waiting on a predecessor whose node has gone NotReady waits
// without a bound — the API server keeps the pod object until a kubelet
// confirms the kill, and there is no kubelet. Failing the Server there makes
// the situation worse rather than better: the replacement is derived from the
// same ordinal name and meets the same pod, a Failed server holds its ordinal
// in DecidePersistentSize's held map, and pruneFailed does not run for a
// persistent group — so the object stays for its full failedRetentionSeconds,
// an hour by default, even after somebody force-deletes the stuck pod. Before
// the deadline existed this Server recovered the moment the name came free.
func TestAnOrdinalWaitingOnItsPredecessorIsNotFailedForHavingNoPod(t *testing.T) {
	f := newFixture(t)
	recreateOrdinalOverATerminatingPod(t, f)

	f.reconcile("survival-0")
	// Far past any bound derived from this group: the point is that the clock
	// does not run here at all, not that it runs slowly.
	f.clock.Advance(2 * time.Hour)
	f.reconcile("survival-0")

	got := f.server("survival-0")
	if got.Status.Phase == string(phase.Failed) {
		t.Errorf("phase = %q while the predecessor's pod is still terminating. The wait is "+
			"somebody else's to end, and failing into it costs an hour of retention on an "+
			"ordinal nothing else can take", got.Status.Phase)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Reason != ReasonPodNameTerminating {
		t.Errorf("Accepted = %+v, want reason %s still — the object must go on naming the "+
			"obstacle for as long as it stands", accepted, ReasonPodNameTerminating)
	}
}

// TestAServerDeletedUnderAPassDoesNotSurfaceAsAnError drives the race the
// recreate path produces: this controller deletes the Server itself, and a
// reconcile holding a cached copy from just before that delete then writes to
// an object the API server no longer has.
//
// Before persistedServer, that NotFound escaped unwrapped and
// controller-runtime logged it at `error` with a stacktrace on a path where
// nothing was wrong -- measured on a real cluster 2026-08-16, with the
// replacement Server created within the second. The reconcile must report the
// disappearance as done, because there is nothing left for it to accomplish
// and the requeued pass already handles the absence at the top of Reconcile.
func TestAServerDeletedUnderAPassDoesNotSurfaceAsAnError(t *testing.T) {
	f := newFixture(t)
	srv := f.createServer("lobby-race")

	// The copy a pass is holding, taken before the object goes away.
	held := srv.DeepCopy()

	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	// The finalizer this fixture's servers carry keeps the object until it is
	// released, so release it and let the delete land for real.
	held2 := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(srv), held2); err == nil {
		held2.Finalizers = nil
		if err := f.c.Update(f.ctx, held2); err != nil {
			t.Fatalf("release finalizer: %v", err)
		}
	}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(srv), &spawneryv1alpha1.Server{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the Server is still there, so this test would prove nothing: %v", err)
	}

	// Exactly what a mid-pass write does: a status update against the stale
	// copy. The bare call is what used to escape; persistedServer is what the
	// reconcile now goes through.
	bare := f.reconc.Status().Update(f.ctx, held)
	if !apierrors.IsNotFound(bare) {
		t.Fatalf("the write did not produce NotFound (%v), so the guard below is "+
			"not being asked the question this test exists for", bare)
	}
	if got := persistedServer(bare); got != nil {
		t.Errorf("persistedServer(%v) = %v, want nil -- a Server that is gone is "+
			"nothing for this pass to do, not an error to log with a stacktrace", bare, got)
	}
}

// TestAnArrivingPlayerKeepsTheDrainingPodAlive is the operator's half of the
// drain gap, driven end to end through the reconciler.
//
// The backend reports zero, freshly, and it is telling the truth: a player
// still completing the configuration phase is counted by neither the backend
// nor the proxy's own player list -- disassembling velocity 3.5.1 build 615,
// VelocityRegisteredServer.addPlayer is called only from
// BackendPlaySessionHandler.activated(), the play phase. So this is exactly
// the state in which a `kubectl delete` used to take the pod out from under
// somebody, and the proxy's attachment report is the only thing that can say
// otherwise.
func TestAnArrivingPlayerKeepsTheDrainingPodAlive(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	// The backend is genuinely empty as far as it can tell.
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	// A proxy of the same namespace says one player is on their way to it.
	proxyUID := "gateway-pod-uid"
	f.agents.Connect(proxyUID, agent.RoleProxy)
	if err := f.agents.ReportBackends(proxyUID, f.ns, map[string]int32{"lobby-x7k2": 1}); err != nil {
		t.Fatalf("ReportBackends: %v", err)
	}

	srv := f.server("lobby-x7k2")
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Draining) {
		t.Fatalf("phase = %q, want Draining", got)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("the pod was deleted with a player arriving on it, which is the whole defect")
	}

	// The player lands and leaves; the proxy says so, and only then does the
	// pod go. This is what keeps the fix from being a permanent hold.
	if err := f.agents.ReportBackends(proxyUID, f.ns, map[string]int32{}); err != nil {
		t.Fatalf("ReportBackends: %v", err)
	}
	f.clock.Advance(time.Second)
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("the pod outlived the last player the proxies knew about")
	}
}

// TestAProxyThatCannotReportBackendsDoesNotHoldEveryServer is the property
// that lets a fleet upgrade in any order, driven where it would actually bite.
//
// An agent too old to send the report says nothing, and reading that as "this
// proxy may be hiding players" would hold every server in the installation
// occupied for as long as one un-upgraded proxy ran -- no drain would ever
// finish anywhere. Silent and old are different states.
func TestAProxyThatCannotReportBackendsDoesNotHoldEveryServer(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	// A proxy that reports players like any agent and knows nothing about
	// backends, which is exactly what a pre-0.2.3 Velocity agent is.
	oldProxy := "old-gateway-uid"
	f.agents.Connect(oldProxy, agent.RoleProxy)
	if err := f.agents.ReportPlayers(oldProxy, 5, 500); err != nil {
		t.Fatalf("ReportPlayers for the proxy: %v", err)
	}

	srv := f.server("lobby-x7k2")
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")
	f.clock.Advance(time.Second)
	f.reconcile("lobby-x7k2")

	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("an empty server was held occupied by a proxy that simply cannot report backends")
	}
}
