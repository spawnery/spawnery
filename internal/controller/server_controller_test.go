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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
			got = blipped.Status.StartedAt.Time.UTC()
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

// TestServerInAPersistentGroupStillDrainsAndReleasesItself pins that "not
// implemented yet" bounds the pod, not the object's lifecycle. Persistent
// groups arrive in milestone 5, so no pod is built for them — but an early
// return on that check would skip the finalizer, the drain and the release
// exactly as the missing-group case used to, and such a Server could never be
// deleted at all.
func TestServerInAPersistentGroupStillDrainsAndReleasesItself(t *testing.T) {
	f := newFixture(t)

	persistent := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:                          spawneryv1alpha1.ServerGroupPersistent,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    50,
			Replicas:                      ptr.To(int32(1)),
			TerminationGracePeriodSeconds: 60,
			FailedRetentionSeconds:        3600,
			Drain:                         &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 60},
			Storage:                       &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("10Gi")},
		},
	}
	if err := f.c.Create(f.ctx, persistent); err != nil {
		t.Fatalf("create persistent ServerGroup: %v", err)
	}

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "survival-0",
			Namespace: f.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       persistent.Name,
				UID:        persistent.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: persistent.Name},
			Ordinal:  ptr.To(int32(0)),
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create Server: %v", err)
	}

	// No pod is built, but the object is managed: finalizer on, reason stated.
	f.reconcile("survival-0")
	srv = f.server("survival-0")
	if _, ok := f.pod("survival-0"); ok {
		t.Fatal("a pod was built for a persistent group")
	}
	if !containsString(srv.Finalizers, ServerFinalizer) {
		t.Fatalf("finalizers = %v, want %s even for a persistent group", srv.Finalizers, ServerFinalizer)
	}
	accepted := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonNotImplemented {
		t.Errorf("Accepted condition = %+v, want False/%s", accepted, spawneryv1alpha1.ReasonNotImplemented)
	}

	// Milestone 5 will create this pod; stand in for it so the drain path is
	// exercised rather than merely the empty release.
	built, err := podspec.BuildServerPod(f.network, persistent, srv)
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	if err := f.c.Create(f.ctx, built); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	f.reconcile("survival-0")
	if got := f.server("survival-0").Status.PodName; got != "survival-0" {
		t.Fatalf("status.podName = %q, want the pod adopted", got)
	}

	pod, ok := f.pod("survival-0")
	if !ok {
		t.Fatal("pod vanished")
	}
	uid := string(pod.UID)
	f.setPodRunning("survival-0", true)
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	if err := f.agents.ReportPlayers(uid, 5, 50); err != nil {
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

	if err := f.agents.ReportPlayers(uid, 0, 50); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("survival-0")
	if _, ok := f.pod("survival-0"); ok {
		t.Fatal("pod leaked: still there after the drain finished")
	}

	f.reconcile("survival-0")
	err = f.c.Get(f.ctx, types.NamespacedName{Name: "survival-0", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer never released on a persistent-group server: %v", err)
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
