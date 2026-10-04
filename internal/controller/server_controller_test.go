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
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// bringUpReady returns the pod UID the agent registry is keyed on.
func bringUpReady(t *testing.T, f *fixture, name string) string {
	t.Helper()
	f.createServer(name)
	return bringUpNamed(t, f, name)
}

// driveToFailed flaps the server past MaxReadinessLosses, leaving its players connected.
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

// setPodFailed leaves the readiness condition as it was, as a kubelet does.
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

// The pod stays Running: the API server evicts a Failed pod without consulting any
// PodDisruptionBudget, so only a crash-looping pod shows what the budget does to a drain.
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

func (f *fixture) pods() []corev1.Pod {
	f.t.Helper()
	list := &corev1.PodList{}
	if err := f.c.List(f.ctx, list, client.InNamespace(f.ns)); err != nil {
		f.t.Fatalf("list pods: %v", err)
	}
	return list.Items
}

func TestTheRoundEndIsStampedWhileTheServerStillRuns(t *testing.T) {
	// The stamp survives an operator restart, so it must be taken before the pod is terminal.
	srv := &spawneryv1alpha1.Server{}
	snap := agent.Snapshot{Known: true, Connected: true, RoundEnded: true}

	changed := stampRoundEnd(srv, snap, time.Unix(1000, 0))

	if !changed || srv.Status.RoundEndedAt == nil {
		t.Fatalf("the round end was not stamped: changed=%v status=%+v", changed, srv.Status)
	}
	first := *srv.Status.RoundEndedAt

	if stampRoundEnd(srv, snap, time.Unix(2000, 0)) {
		t.Error("the stamp moved on a second pass; it must be taken once")
	}
	if !srv.Status.RoundEndedAt.Equal(&first) {
		t.Errorf("the stamp changed: %v then %v", first, *srv.Status.RoundEndedAt)
	}
}

// Ready -> Failed directly: entering Starting would clear readySince on its own.
func TestServerFailedStraightFromReadyClearsReadySince(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")
	if f.server("lobby-x7k2").Status.ReadySince == nil {
		t.Fatal("status.readySince was not stamped at Ready; the clearing below would prove nothing")
	}

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

// status.startedAt is never refreshed, so a blip on a long-running server must not fail it.
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

	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")
	blipped := f.server("lobby-x7k2")
	if got := blipped.Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q after the blip, want Starting", got)
	}
	// Entering Starting re-arms the startup deadline.
	if blipped.Status.StartedAt == nil || !blipped.Status.StartedAt.Time.Equal(f.clock.Now()) {
		var got any
		if blipped.Status.StartedAt != nil {
			got = blipped.Status.StartedAt.UTC()
		}
		t.Fatalf("status.startedAt = %v, want it re-armed to %v on entry into Starting",
			got, f.clock.Now().UTC())
	}

	f.reconcile("lobby-x7k2")
	srv := f.server("lobby-x7k2")
	if srv.Status.Phase == string(phase.Failed) {
		t.Fatal("a long-lived server was failed by its stale startup deadline")
	}
	if srv.Status.Phase != string(phase.Starting) {
		t.Fatalf("phase = %q, want Starting", srv.Status.Phase)
	}

	f.setPodRunning("lobby-x7k2", true)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase = %q after recovery, want Ready", got)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while 40 players were online — core invariant broken")
	}
}

func TestServerThatNeverBecomesPlayableFailsAtTheDeadline(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

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

// failLateStarter leaves the server Failed with its pod still running.
func failLateStarter(t *testing.T, f *fixture, name string) {
	t.Helper()
	f.createServer(name)
	f.reconcile(name)
	f.setPodRunning(name, false)
	f.reconcile(name)
	f.clock.Advance(6 * time.Minute) // the fixture's deadline is 5 minutes
	f.reconcile(name)
	if got := f.server(name).Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q past the startup deadline, want Failed", got)
	}
}

func TestAFailedServersLatePodIsStoppedOnceItsGroupHasAReadyServer(t *testing.T) {
	f := newFixture(t)
	failLateStarter(t, f, "lobby-late")
	bringUpReady(t, f, "lobby-good")

	f.setPodRunning("lobby-late", true)
	f.reconcile("lobby-late")

	if pod, ok := f.pod("lobby-late"); ok && pod.DeletionTimestamp.IsZero() {
		t.Error("the late pod of a failed server keeps running beside a ready server")
	}
	if got := f.server("lobby-late").Status.Phase; got != string(phase.Failed) {
		t.Errorf("phase = %q, want Failed: the object stays for diagnosis", got)
	}
}

func TestAFailedServersLatePodStaysWhileItsGroupHasNoReadyServer(t *testing.T) {
	f := newFixture(t)
	failLateStarter(t, f, "lobby-late")

	f.setPodRunning("lobby-late", true)
	f.reconcile("lobby-late")

	if pod, ok := f.pod("lobby-late"); !ok || !pod.DeletionTimestamp.IsZero() {
		t.Error("the only running pod of the group was stopped")
	}
}

// The flap counter never sees a permanently red probe, so only the re-armed deadline fails it.
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

// The deadline is re-armed only on entry into Starting; re-arming every pass would push it out forever.
func TestZombieIsCaughtUnderAContinuousReconcileLoop(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	f.setPodRunning("lobby-x7k2", false)

	const ticks = 200 // 200 * 5s resync is far past the 5 minute deadline
	failedAfter := -1
	for i := 0; i < ticks; i++ {
		f.clock.Advance(ResyncInterval)
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
			srv.Status.Phase, ticks, time.Duration(ticks)*ResyncInterval)
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

	// The real registrar fans out to every proxy, so the command must not be re-broadcast each pass.
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

	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("pod of an empty failed server survived its retention")
	}
}

// With a 60s drain timeout and an hour's retention, the drain deadline has always passed by then.
func TestFailedServerIsCleanedUpOnceItsDrainDeadlinePasses(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	driveToFailed(t, f, "lobby-x7k2")

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

	// The Failed branch returns StartDrain on every pass here; the command must still go out once.
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

	// A report sharing an instant with the drain decision cannot answer it; see CountPredatesDrain.
	f.clock.Advance(2 * time.Second)
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

	// A report sharing an instant with the drain decision cannot answer it; see CountPredatesDrain.
	f.clock.Advance(2 * time.Second)
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

	// A report sharing an instant with the drain decision cannot answer it; see CountPredatesDrain.
	f.clock.Advance(2 * time.Second)
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

	// Asked after the object is released, the state a recreated ordinal arrives in.
	if f.claim("survival-0-data") == nil {
		t.Error("the claim went with the server it outlived; the world is gone")
	}
}

// Without adoption podName and startedAt stay empty, so neither the deadline nor PodLost could fire.
func TestPodIsAdoptedAfterALostStatusWrite(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod created")
	}
	originalUID := pod.UID

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

	f.setPodRunning("lobby-x7k2", true)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q with a green probe alone, want Starting", got)
	}
	if len(f.registrar.registered) != 0 {
		t.Errorf("registered = %v, want no registration before the agent is ready", f.registrar.registered)
	}

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

// Nothing tells the agent registry to forget a pod, so a crashed pod keeps reporting its last count.
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

	f.setPodFailed("lobby-x7k2")
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q after the pod reached PodFailed, want Failed", got)
	}

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
		f.clock.Advance(ResyncInterval)
	}
}

// An untrusted count hides players only on a server the proxies route to.
func TestOccupiedLabelNeedsTheServerToHaveBeenRegistered(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	// No green probe and no agent: the registry knows nothing, so the count is stale.
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
		f.clock.Advance(ResyncInterval)
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

// Update writes the persisted, still empty status back over the object, dropping any condition set before ensureFinalizer.
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

	f.clock.Advance(time.Second)
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}

	// A count taken before the drain cannot say whether it finished; see CountPredatesDrain.
	f.clock.Advance(2 * time.Second)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod still there after the drain finished")
	}

	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Server still present after the drain: %v", err)
	}
}

// Deregistering only stops new joins, so a once-registered Starting server still has players.
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

// PodTerminal aborts a running drain, so a crash-looping sidecar must not cut one short.
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

// denyingCreator refuses every create permanently, as an admission webhook or quota would.
type denyingCreator struct {
	client.Client
}

func (denyingCreator) Create(context.Context, client.Object, ...client.CreateOption) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "configmaps"},
		podspec.CAConfigMapName,
		errors.New("denied by an admission policy"))
}

// status.startedAt needs a pod, so without a condition such a Server would sit in Pending silently.
func TestReconcileReportsANamespaceItCannotBootstrap(t *testing.T) {
	f := newFixture(t)

	// newFixture already bootstrapped the namespace; without removing these the denying Bootstrapper never calls Create.
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

// A deletion before wasRegistered is durable would terminate without draining, so it is read back
// from the API server inside Register.
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

// wasRegistered stays true after a failed Register: the proxies may have been told about an earlier one.
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

// The API server silently drops unknown fields, so only a real round trip catches a missed make manifests.
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

// retiringFor builds a Server that has been Retiring for d.
func retiringFor(d time.Duration) *spawneryv1alpha1.Server {
	since := metav1.NewTime(time.Now().Add(-d))
	return &spawneryv1alpha1.Server{
		Status: spawneryv1alpha1.ServerStatus{
			Phase:         string(phase.Retiring),
			RetiringSince: &since,
		},
	}
}

func groupWithMaxStale(seconds int32) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Update: &spawneryv1alpha1.UpdateSpec{MaxStaleSeconds: seconds},
		},
	}
}

// collectInputsReconciler has no client, so it only works against objects built in memory.
func collectInputsReconciler(clock func() time.Time) *ServerReconciler {
	return &ServerReconciler{
		Clock:  clock,
		Agents: agent.New(clock, 5*time.Second, clock()),
	}
}

func TestMaxStaleZeroNeverForcesADrain(t *testing.T) {
	// The default: a retiring server with players waits indefinitely.
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

// Re-stamping retiringSince on every pass would keep the maxStaleSeconds deadline from ever arriving.
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

	// MaxStaleSeconds is 0 here, so only the timestamp is under test.
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

// The escalation must go through the drain path, never straight to Terminating.
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

func TestMaxStaleZeroLeavesARetiringServerAloneIndefinitely(t *testing.T) {
	f := newFixture(t)
	// Explicit so the zero case does not depend on the fixture's default.
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

// createPersistentServer builds the Server as ServerGroupReconciler.createPersistentServer does.
// Controller: true matters: the adoption path asks metav1.IsControlledBy.
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

// claim returns nil for an absent claim, and one with a deletion timestamp counts as absent:
// envtest never clears the pvc-protection finalizer.
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

// envtest runs no provisioner, so the claim never binds and the pod never runs.
// Swapping the two Creates passes this test; it catches a missing claim or pod.
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

// With no kubelet a bound pod keeps its deletion timestamp; a force delete stands in for the kubelet's confirmation.
func (f *fixture) retirePodTheWayAKubeletWould(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	current := &corev1.Pod{}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		t.Fatalf("get pod %s: %v", pod.Name, err)
	}
	if controllerutil.RemoveFinalizer(current, testPodHold) {
		if err := f.c.Update(f.ctx, current); err != nil {
			t.Fatalf("release pod %s: %v", pod.Name, err)
		}
	}
	if err := f.c.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("force delete pod %s: %v", pod.Name, err)
	}
}

const testPodHold = "spawnery.cloud/test-hold"

// Binding alone is not enough: under load the API server occasionally deletes a just-bound pod with grace period 0.
func (f *fixture) holdPodOnDelete(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	current := &corev1.Pod{}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		t.Fatalf("get pod %s: %v", pod.Name, err)
	}
	controllerutil.AddFinalizer(current, testPodHold)
	if err := f.c.Update(f.ctx, current); err != nil {
		t.Fatalf("hold pod %s: %v", pod.Name, err)
	}
}

// recreateOrdinalOverATerminatingPod replaces an ordinal's Server while its predecessor's pod is
// still terminating, and returns that pod. The API server force-deletes an unscheduled pod, so it is bound first.
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
	f.holdPodOnDelete(t, pod)
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

// A create into a name still held gets AlreadyExists; recording that pod would read it as lost
// and delete this Server, in a loop no failure count sees.
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

// The pod name is reused across generations, so only the UID tells them apart.
func podUnderNameIsStill(f *fixture, name string, uid types.UID) bool {
	pod := &corev1.Pod{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: name}, pod); err != nil {
		return false
	}
	return pod.UID == uid
}

// Its podName assertion has failed once, unexplained; see docs/reference/known-issues.md.
func TestARecreatedOrdinalCreatesItsPodOnceThePredecessorIsGone(t *testing.T) {
	f := newFixture(t)
	terminating := recreateOrdinalOverATerminatingPod(t, f)
	f.reconcile("survival-0")

	f.retirePodTheWayAKubeletWould(t, terminating)
	f.reconcile("survival-0")

	got := f.server("survival-0")
	if got.Status.PodName != "survival-0" {
		// A lingering predecessor says the force delete did not take,
		// PodNameTerminating with no such pod says the controller decided
		// against a pod that is gone, and an empty namespace with a clean
		// condition says something else refused the create.
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
	// Accepted must clear rather than keep a reason that outlived the wait.
	accepted := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v once the pod was created, want True: the wait has to clear", accepted)
	}
}

// resourceVersion catches any write; the field assertions cannot fail while it holds and
// say what it protects.
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

// growClaim runs every pass, since nothing in pod creation runs once the pod exists.
// envtest resizes only a Bound claim, and nothing binds one, so the bind is faked by hand.
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

	// storageClassName is immutable once set, so the group is built by hand with it.
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

// envtest runs no CSI driver, so FileSystemResizePending is written by hand; this tests the reaction only.
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
	// Narrowing the guard to == 0 would attempt a refused shrink and still pass the assertions above.
	if srv := f.server("survival-0"); srv.Status.StorageResizeError != "" {
		t.Errorf("storageResizeError = %q, want empty; nothing should have been sent to this claim",
			srv.Status.StorageResizeError)
	}
}

// A deleted and remade claim reads as absent, because envtest never clears pvc-protection.
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

// claimsMounted reads the pod rather than trusting BuildServerPod.
func claimsMounted(pod *corev1.Pod) []string {
	var names []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			names = append(names, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return names
}

// envtest runs no garbage collector, so this cannot catch an owner reference.
// The second reconcile is the one that releases the finalizer.
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

// refusingPodCreator refuses only pods: the pass writes a ConfigMap, ServiceAccounts and a PVC first.
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

// Returning the error alone leaves no podName and no startedAt, so no deadline would ever fire.
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
	// f.reconcile fatals on error: a policy refusal is reported, not retried.
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

// A unit test: through the API server it would prove only no-ops.
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
	if got := fallbackGroup(persistent); got.Spec.Type != spawneryv1alpha1.ServerGroupPersistent {
		t.Errorf("a Server carrying spec.ordinal falls back to %q, want Persistent. "+
			"createPersistentServer is the only thing that sets that field, so it is "+
			"what identifies the type of a group that is no longer there to ask",
			got.Spec.Type)
	}

	// Ordinal 0 exists in every persistent group, so a value check would misread it.
	if got := fallbackGroup(persistent); *persistent.Spec.Ordinal != 0 ||
		got.Spec.Type != spawneryv1alpha1.ServerGroupPersistent {
		t.Errorf("ordinal 0 read as ephemeral: %q", got.Spec.Type)
	}

	if a, b := fallbackGroup(ephemeral), fallbackGroup(persistent); a.DrainTimeout() != b.DrainTimeout() ||
		a.FailedRetention() != b.FailedRetention() || a.UpdateMaxStale() != b.UpdateMaxStale() {
		t.Errorf("the fallback timings differ by type: ephemeral %v/%v/%v, persistent %v/%v/%v",
			a.DrainTimeout(), a.FailedRetention(), a.UpdateMaxStale(),
			b.DrainTimeout(), b.FailedRetention(), b.UpdateMaxStale())
	}
}

func TestTheFallbackGroupOfAnOnDemandMemberIsOnDemand(t *testing.T) {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-c0ffee", Namespace: "mc"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"},
			Key:      "c0ffee",
		},
	}
	got := fallbackGroup(srv)
	if got.Spec.Type != spawneryv1alpha1.ServerGroupOnDemand {
		t.Fatalf("type = %q, want OnDemand: a member whose group is gone must not read as ephemeral, "+
			"or its claim is skipped", got.Spec.Type)
	}
	if got.IsEphemeral() {
		t.Error("an on-demand member's fallback group reports itself ephemeral")
	}
}

// The table is hand-written: a type added to the enum needs its row here.
func TestTheFallbackGroupCoversEveryType(t *testing.T) {
	marker := map[spawneryv1alpha1.ServerGroupType]func(*spawneryv1alpha1.Server){
		spawneryv1alpha1.ServerGroupEphemeral:  func(*spawneryv1alpha1.Server) {},
		spawneryv1alpha1.ServerGroupPersistent: func(s *spawneryv1alpha1.Server) { s.Spec.Ordinal = ptr.To[int32](0) },
		spawneryv1alpha1.ServerGroupOnDemand:   func(s *spawneryv1alpha1.Server) { s.Spec.Key = "c0ffee" },
	}
	for want, mark := range marker {
		srv := &spawneryv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: "minecraft"},
			Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
		}
		mark(srv)
		if got := fallbackGroup(srv).Spec.Type; got != want {
			t.Errorf("a Server marked for %s falls back to %q", want, got)
		}
	}
}

// Between the startup and creation deadlines it must stay Pending: it never had a pod, so it never failed to become ready.
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

	// Past the creation deadline: drain timeout plus startup deadline.
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

// Failing a Server waiting on a NotReady node's pod would hold the ordinal for the full failedRetentionSeconds.
func TestAnOrdinalWaitingOnItsPredecessorIsNotFailedForHavingNoPod(t *testing.T) {
	f := newFixture(t)
	recreateOrdinalOverATerminatingPod(t, f)

	f.reconcile("survival-0")
	// The clock must not run here at all.
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

// The requeued pass handles the absence, so NotFound here is done, not an error.
func TestAServerDeletedUnderAPassDoesNotSurfaceAsAnError(t *testing.T) {
	f := newFixture(t)
	srv := f.createServer("lobby-race")

	held := srv.DeepCopy()

	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
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

// Velocity counts a player only in the play phase, so the backend's fresh zero misses one still configuring.
func TestAnArrivingPlayerKeepsTheDrainingPodAlive(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
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

	// metav1.Time keeps whole seconds, so a report must clear the stamp plus one.
	f.clock.Advance(2 * time.Second)
	if err := f.agents.ReportBackends(proxyUID, f.ns, map[string]int32{}); err != nil {
		t.Fatalf("ReportBackends: %v", err)
	}
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("the pod outlived the last player the proxies knew about")
	}
}

// An agent too old to report backends must not hold every server occupied.
func TestAProxyThatCannotReportBackendsDoesNotHoldEveryServer(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	// What a pre-0.2.3 Velocity agent is: it reports players and knows nothing about backends.
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
	// Past the drain stamp with a fresh count, so only the proxy could hold the pod.
	f.clock.Advance(2 * time.Second)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("second ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("an empty server was held occupied by a proxy that simply cannot report backends")
	}
}

// A fresh count taken before the drain says nothing about a player who joined since.
func TestADrainWaitsForACountTakenAfterItStarted(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	f.clock.Advance(time.Second)
	srv := f.server("lobby-x7k2")
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Draining) {
		t.Fatalf("phase = %q, want Draining", got)
	}

	f.clock.Advance(time.Second)
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("the pod went on a count taken before the drain began")
	}

	f.clock.Advance(2 * time.Second)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("second ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("the pod outlived a report that said the server was empty since the drain began")
	}
}

func TestTheDrainDeadlineStillEndsAWaitNobodyCanSatisfy(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.clock.Advance(time.Second)

	srv := f.server("lobby-x7k2")
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")
	f.agents.Disconnect(uid)

	f.clock.Advance(2 * time.Second)
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("the pod went before the deadline, so this test would prove nothing about it")
	}

	f.clock.Advance(2 * time.Minute)
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("the drain deadline did not end a wait nobody could satisfy")
	}
}

// A hard-powered-off node: the stream still reads connected, and the reports have stopped.
func TestADeadBackendIsDrainedBeforeVelocityKicksItsPlayers(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q, want Ready", got)
	}

	f.clock.Advance(30 * time.Second)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase == string(phase.Ready) {
		t.Error("a server whose agent went silent is still Ready, so players keep being routed to it")
	}
	if srv.Status.Registered {
		t.Error("it is still registered with the proxies")
	}
	// Velocity kicks those players outright when its read timeout fires.
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want one drain command before the read timeout", f.registrar.drained)
	}
}

// This rule runs against every Ready server on every pass.
func TestALiveAgentThatKeepsReportingIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")

	for i := 0; i < 4; i++ {
		if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.clock.Advance(5 * time.Second)
		f.reconcile("lobby-x7k2")
	}

	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase = %q, want Ready: a reporting agent was read as silent", got)
	}
	if len(f.registrar.drained) != 0 {
		t.Errorf("drained = %v, want none", f.registrar.drained)
	}
}

// A Create that returned AlreadyExists has no UID, so the UID is taken wherever the pod is seen.
func TestAServerRecordsWhichPodItIsRunning(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")

	recorded := f.server("lobby-x7k2").Status.PodUID
	if recorded == "" {
		t.Fatal("status.podUID is empty on a Ready server, so nothing can tell one run from another")
	}

	pod, found := f.pod("lobby-x7k2")
	if !found {
		t.Fatal("no pod for a Ready server")
	}
	if recorded != string(pod.UID) {
		t.Errorf("status.podUID = %q, want the pod's own %q", recorded, pod.UID)
	}
}

// Defaults are read from the API source, so a new field fails here rather than running on Go's zero value.
func TestTheFallbackGroupCarriesEveryCrdDefault(t *testing.T) {
	src, err := os.ReadFile("../../api/v1alpha1/servergroup_types.go")
	if err != nil {
		t.Fatalf("reading the API source the defaults live in: %v", err)
	}
	text := string(src)

	scalarDefault := func(field string) int32 {
		t.Helper()
		// The marker nearest above the field declaration, which is how
		// controller-gen reads it too.
		decl := regexp.MustCompile(`(?s)\+kubebuilder:default=(\d+)[^\n]*\n(?:\s*//[^\n]*\n)*\s*` +
			field + `\s+int(?:32|64)\s+` + "`")
		m := decl.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("no +kubebuilder:default marker found above %s; if the field was "+
				"renamed or its default removed, this test and fallbackGroup both need it", field)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s default %q is not a number: %v", field, m[1], err)
		}
		return int32(n)
	}

	drain := regexp.MustCompile(`\+kubebuilder:default=\{timeoutSeconds:(\d+)\}`).FindStringSubmatch(text)
	if drain == nil {
		t.Fatal("no +kubebuilder:default for spec.drain found in the API source")
	}
	drainSeconds, err := strconv.Atoi(drain[1])
	if err != nil {
		t.Fatalf("drain default %q is not a number: %v", drain[1], err)
	}

	want := map[string]time.Duration{
		"drain.timeoutSeconds":     time.Duration(drainSeconds) * time.Second,
		"failedRetentionSeconds":   time.Duration(scalarDefault("FailedRetentionSeconds")) * time.Second,
		"finishedRetentionSeconds": time.Duration(scalarDefault("FinishedRetentionSeconds")) * time.Second,
	}

	for _, srv := range []*spawneryv1alpha1.Server{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: "minecraft"},
			Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft"},
			Spec: spawneryv1alpha1.ServerSpec{
				GroupRef: spawneryv1alpha1.ObjectRef{Name: "survival"},
				Ordinal:  ptr.To[int32](0),
			},
		},
	} {
		g := fallbackGroup(srv)
		got := map[string]time.Duration{
			"drain.timeoutSeconds":     g.DrainTimeout(),
			"failedRetentionSeconds":   g.FailedRetention(),
			"finishedRetentionSeconds": g.FinishedRetention(),
		}
		for field, w := range want {
			if got[field] != w {
				t.Errorf("fallbackGroup(%s).%s = %v, want the CRD default %v",
					srv.Name, field, got[field], w)
			}
		}
	}
}

// netstate hands the status to the connect router, so a raw absurd capacity would win every move.
func TestTheMirroredCountIsClampedToTheGroupsCapacity(t *testing.T) {
	r := &ServerReconciler{PlayerStatusInterval: time.Minute}
	srv := &spawneryv1alpha1.Server{}
	r.mirrorPlayerCount(srv, agent.Snapshot{Known: true, Players: 0, Slots: 1 << 30}, 80, nil,
		metav1.NewTime(time.Now()))
	if srv.Status.Slots != 80 {
		t.Errorf("status.slots = %d, want the group's 80", srv.Status.Slots)
	}

	// fallbackGroup has no maxPlayers, and clampReport would floor that to 1.
	gone := &spawneryv1alpha1.Server{}
	r.mirrorPlayerCount(gone, agent.Snapshot{Known: true, Players: 15, Slots: 20}, 0, nil,
		metav1.NewTime(time.Now()))
	if gone.Status.Players != 15 || gone.Status.Slots != 20 {
		t.Errorf("status = %d/%d without a group, want the report's 15/20 unclamped",
			gone.Status.Players, gone.Status.Slots)
	}
}

func TestTheMirroredCountCarriesThePlayableFigure(t *testing.T) {
	r := &ServerReconciler{PlayerStatusInterval: time.Minute}
	srv := &spawneryv1alpha1.Server{}
	spec := ptr.To[int32](12)

	r.mirrorPlayerCount(srv, agent.Snapshot{Known: true, Players: 14, Slots: 100}, 100, spec,
		metav1.NewTime(time.Now()))
	if srv.Status.PlayableSlots != 12 || srv.Status.Players != 14 {
		t.Errorf("status = %d players, %d playable; want 14 and the spec's 12",
			srv.Status.Players, srv.Status.PlayableSlots)
	}

	r.mirrorPlayerCount(srv, agent.Snapshot{Known: true, Players: 14, Slots: 100, PlayableSlots: 8}, 100, spec,
		metav1.NewTime(time.Now()))
	if srv.Status.PlayableSlots != 8 {
		t.Errorf("playable = %d, want the plugin's 8 mirrored at once", srv.Status.PlayableSlots)
	}

	gone := &spawneryv1alpha1.Server{}
	r.mirrorPlayerCount(gone, agent.Snapshot{Known: true, Players: 15, Slots: 20}, 0, nil,
		metav1.NewTime(time.Now()))
	if gone.Status.PlayableSlots != 20 {
		t.Errorf("playable = %d without a group, want the report's 20", gone.Status.PlayableSlots)
	}
}

func (f *fixture) forceStop(t *testing.T, name string) {
	t.Helper()
	srv := f.server(name)
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.ForceStop = true
	if err := f.c.Patch(f.ctx, srv, patch); err != nil {
		t.Fatalf("set spec.forceStop on %s: %v", name, err)
	}
}

func (f *fixture) podAnyway(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, pod); err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod
}

func TestAForceStopKillsThePodWithoutAGracePeriod(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")
	pod, _ := f.pod("lobby-x7k2")
	f.bindPodToNode(t, pod, f.ensureNode(t, "node-force-"+f.ns, false).Name)
	f.holdPodOnDelete(t, pod)
	t.Cleanup(func() { f.retirePodTheWayAKubeletWould(t, pod) })

	f.forceStop(t, "lobby-x7k2")
	f.reconcile("lobby-x7k2")

	got := f.podAnyway(t, "lobby-x7k2")
	if got.DeletionTimestamp.IsZero() {
		t.Fatal("the pod was not deleted")
	}
	if g := got.DeletionGracePeriodSeconds; g == nil || *g != 0 {
		t.Errorf("deletionGracePeriodSeconds = %v, want 0", g)
	}
	if phase.Phase(f.server("lobby-x7k2").Status.Phase) != phase.Terminating {
		t.Errorf("phase = %s, want Terminating", f.server("lobby-x7k2").Status.Phase)
	}
	if !slices.Contains(f.registrar.deregistered, "lobby-x7k2") {
		t.Errorf("deregistered = %v, want the server taken out of the proxies", f.registrar.deregistered)
	}
	if len(f.registrar.drained) != 0 {
		t.Errorf("drained = %v, want no drain", f.registrar.drained)
	}
}

func TestAForceStopShortensAGracePeriodAlreadyRunning(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")
	pod, _ := f.pod("lobby-x7k2")
	f.bindPodToNode(t, pod, f.ensureNode(t, "node-force-"+f.ns, false).Name)
	f.holdPodOnDelete(t, pod)
	t.Cleanup(func() { f.retirePodTheWayAKubeletWould(t, pod) })
	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete the pod gracefully: %v", err)
	}
	if g := f.podAnyway(t, "lobby-x7k2").DeletionGracePeriodSeconds; g == nil || *g == 0 {
		t.Fatalf("the ordinary delete left grace %v; the test needs a running grace period", g)
	}

	f.forceStop(t, "lobby-x7k2")
	f.reconcile("lobby-x7k2")

	if g := f.podAnyway(t, "lobby-x7k2").DeletionGracePeriodSeconds; g == nil || *g != 0 {
		t.Errorf("deletionGracePeriodSeconds = %v, want 0 after the force-stop", g)
	}
}
