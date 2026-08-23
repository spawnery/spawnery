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
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/render"
)

// ProxyDrainingSinceAnnotation records when the operator first asked a proxy
// pod to stop taking connections, as an RFC 3339 timestamp.
//
// It is on the pod because that is the only per-pod place that survives an
// operator restart: a proxy has no CR of its own, and the ProxyGroup's status
// is per group. Everything else about a drain is re-derived every pass — which
// pods are surplus, and therefore what readiness each should have — so this is
// the only thing that has to be written down.
const ProxyDrainingSinceAnnotation = "spawnery.cloud/draining-since"

// readinessDivergenceGrace is how long a pod's actual readiness may disagree
// with the asserted one before the group says so. It must clear both known
// delays: the kubelet's probe takes 10 to 15 seconds to flip a condition
// (period 5s times failure threshold 3), and Fleet.Resync re-asserts every 30
// seconds. 60 clears both with margin.
const readinessDivergenceGrace = 60 * time.Second

// ProxyReadinessSetter is how the ProxyGroup controller tells one proxy pod
// whether it should be taking connections. *proxyreg.Fleet satisfies it; the
// narrow interface — not the concrete type — mirrors why Registrar exists for
// the Server controller's wider write surface, and keeps this file's only
// dependency on proxyreg to the one method it actually calls.
type ProxyReadinessSetter interface {
	SetReady(ctx context.Context, podUID string, ready bool) error
}

// NewProxyName builds a unique proxy pod name below the group prefix. Same
// generator and same alphabet as NewServerName: a proxy has no CR of its own,
// so the pod name is the only handle anyone has on it, and it has to be as
// readable off a terminal as a server's.
func NewProxyName(group string) string { return NewServerName(group) }

// ProxyGroupReconciler keeps a proxy group at its replica count, keeps its
// Service in step, and publishes where players connect.
//
// Unlike ServerGroupReconciler it manages pods directly. Proxies are
// fungible: there is no per-proxy object and no state machine. Draining one
// is the readiness contract — telling a surplus pod's own agent to stop
// taking connections and dating when that started — plus the wait that
// contract exists for: reconcileReplicas removes a surplus pod once it is
// empty, or once its deadline has passed, and not before.
//
// It emits Kubernetes events on four occasions, and the bar all four clear
// is the same one: something happened that no other signal on the object
// reports.
//
//   - A proxy marked for going off a departing node (NodeDraining, Normal,
//     from the assertion loop in reconcileReplicas). Fired once, on the pass
//     that actually marks it, and only when the node is the reason — a
//     hash-mismatch-only surplus reduction says nothing here.
//   - A drain that ran out of time (ProxyDrainTimeout, Warning, from the
//     deletion loop in reconcileReplicas). It is the one thing in this
//     milestone that disconnects a player, it is configured rather than
//     accidental, and nothing else on the object would ever say it happened.
//   - A readiness divergence crossing its grace period, on the flank in
//     either direction (ReadinessDiverged as a Warning, ReadinessAgrees as a
//     Normal, from reportReadinessDivergence). Here the condition of the same
//     name carries the state, so what the event adds is the transition and
//     its timing: a condition read later says a proxy is ignoring its
//     withdrawal, not when it started or that it happened at all if it has
//     since cleared. An agent that heard a withdrawal and went on taking
//     connections looks healthy from every angle the kubelet reports, so
//     nothing outside this pair reports it.
//   - A proxy pod that cannot come into existence, on the flank in either
//     direction (ProxyPodBlocked as a Warning, from either the create path in
//     Reconcile when the API server refuses a pod or the scheduler-reading
//     branch of reportBlockedProxies when one cannot be placed; and the
//     recovery back to every proxy pod existing, ProxyPodsAdmitted as a
//     Normal, from reportBlockedProxies). Same shape as the readiness pair
//     above: the Degraded condition already carries which of the two is
//     true, so what the event adds is that a transition happened and when —
//     a group that recovers between two resyncs would otherwise show a
//     clean Degraded=False with nothing on its timeline saying it was ever
//     anything else.
//
// Pod creation and ordinary deletion stay silent: they are recorded on the
// objects themselves and an event would say nothing the group's status does
// not.
type ProxyGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Agents is the runtime state reported by the in-game agents. Read for the
	// connected player count.
	Agents *agent.Registry
	// Bootstrap puts the CA bundle and the ServiceAccounts into the namespace
	// before the first pod is created there.
	Bootstrap *Bootstrapper
	// AgentEndpoint is the address the in-game agent dials.
	AgentEndpoint string
	// Proxies is how a surplus proxy is told to stop taking connections, and
	// a proxy that is no longer surplus is told to resume. See
	// ProxyReadinessSetter.
	Proxies ProxyReadinessSetter
	// Clock is injectable so the drain deadline is testable.
	Clock func() time.Time
	// Recorder announces two things: a drain that hit its deadline with
	// players still on the proxy, and the flank of a readiness divergence.
	// See the type comment for the bar both clear and why nothing else here
	// is announced.
	Recorder events.EventRecorder
	// Expectations reserves the pod creates and deletes this reconciler has
	// issued and the cache has not shown yet. One instance is shared across
	// groups. Task 4 made a rollout create a pod per replacement rather than
	// only at scale-up, which makes the create path race the informer cache as
	// a matter of course rather than rarely; see expectations.go. Only create
	// and delete apply here -- a proxy has no per-pod CR and so no retirement,
	// which is the one thing ServerGroupReconciler reserves that this does not.
	Expectations *expectations
	// Divergence tracks how long each pod's actual readiness has disagreed
	// with the readiness this reconciler last asserted for it, so
	// ConditionReadinessDiverged can be raised once that has run past
	// readinessDivergenceGrace. One instance is shared across groups, like
	// Expectations.
	Divergence *readinessDivergence
	// DrainTaintKeys is Options.DrainTaintKeys. Nil means only cordoned nodes
	// count.
	DrainTaintKeys []string
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;delete

// Reconcile brings one ProxyGroup in line with its spec.
func (r *ProxyGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	group := &spawneryv1alpha1.ProxyGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		if apierrors.IsNotFound(err) {
			// No ProxyGroup finalizer exists, so a deleted group is gone from
			// the API server before any reconcile can observe its deletion
			// timestamp. This is the only path most deletions take, and without
			// it a group's reservations would sit under a key nothing will ever
			// observe again, for the life of the process -- see
			// ServerGroupReconciler.Reconcile, which forgets for the same
			// reason at the same two sites. Divergence forgets here too, for
			// the same reason: its pods are gone with the group, and no later
			// observe call for this key is ever coming to prune their entries.
			r.Expectations.forget(req.Namespace + "/" + req.Name)
			r.Divergence.forget(req.Namespace + "/" + req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// The pods and the Service are owned by this object, so the API server
		// removes them. There is nothing to drain here: moving players is the
		// proxy's own job, and making deletion wait for it belongs to a later
		// task.
		r.Expectations.forget(group.Namespace + "/" + group.Name)
		r.Divergence.forget(group.Namespace + "/" + group.Name)
		return ctrl.Result{}, nil
	}

	network := &spawneryv1alpha1.Network{}
	key := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: group.Namespace}
	switch err := r.Get(ctx, key, network); {
	case apierrors.IsNotFound(err):
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonNetworkNotFound,
			fmt.Sprintf("Network %q does not exist", group.Spec.NetworkRef.Name))
		return r.refuse(ctx, group)
	case err != nil:
		return ctrl.Result{}, err
	}
	if !meta.IsStatusConditionTrue(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonNetworkNotAccepted,
			networkNotAcceptedMessage(network))
		return r.refuse(ctx, group)
	}

	// The strategies differ in reconcileService and proxyAddress and nowhere
	// else. This guard is not about any of them: the CRD's enum is closed, so
	// no object carrying an unrecognised type can be created, and the branch
	// below is reachable only if a fifth value is added to the enum without
	// a branch to serve it. A refusal on the object is a message a user can
	// read; the alternative is now reconcileService's named error, which
	// fails the reconcile into the log where nobody looking at the group
	// would find it. (Before this operator gained a ClusterIP branch that
	// alternative was a nil dereference on an unvalidated sub-block. It is
	// not any more, and this guard's value is the reporting, not the crash
	// it used to prevent.)
	if !exposeImplemented(group.Spec.Expose.Type) {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonExposeNotImplemented,
			fmt.Sprintf("expose.type %s is not implemented by this operator",
				group.Spec.Expose.Type))
		// refuse rather than a bare return, for the reason it documents: the
		// player-safety pass has to run again, and whether a proxy is
		// occupied is a fact about the agent registry that nothing watches.
		// A user whose group is stuck here has real pods with real players on
		// them for as long as the refusal stands.
		return r.refuse(ctx, group)
	}
	setProxyGroupAccepted(group, true, spawneryv1alpha1.ReasonAccepted, "")
	// Persisted now, before any of the side effects below can fail: without
	// this write, a group that reaches here and then hits an error — the
	// shipped sample's hardcoded NodePort colliding across namespaces is a
	// real way to do it — would return having recorded nothing, leaving the
	// object with no conditions and no phase, indistinguishable from one no
	// reconcile has ever touched. Recording the intent before attempting the
	// side effect is the same shape as persisting status.wasRegistered before
	// telling the proxies elsewhere in this plan: a failure partway through
	// must not leave the record claiming less than what is actually true.
	if err := r.writeStatus(ctx, group); err != nil {
		return ctrl.Result{}, err
	}

	obs, res, err := r.reconcileObserved(ctx, network, group)
	// The status is written wherever the pods and the Service were actually
	// read, and nowhere else. Both halves of that are deliberate.
	//
	// Wherever: before this, setStatus was the last call before the successful
	// return and eleven error returns stood in front of it, so a pass that
	// failed partway through kept whatever the last successful pass had
	// published. A group switched into a strategy the API server refuses went
	// on advertising the node port of a Service that reconcileService had
	// already deleted, and no later pass corrected it because no later pass
	// could get any further.
	//
	// And nowhere else: Reconcile also returns before any of this, on a
	// missing or unaccepted Network and on an unimplemented expose type. The
	// address is untouched there on purpose. Nothing about the serving world
	// has changed on those paths -- the pods are running, the Service is
	// there, people are connected through it -- and blanking a working address
	// because a different object went missing would be a regression caused by
	// this fix rather than a part of it.
	if obs.observed {
		r.setStatus(group, obs.pods, obs.svc)
		if werr := r.writeStatus(ctx, group); werr != nil {
			if err == nil {
				return res, werr
			}
			// The reconcile's own error is the cause and the one worth backing
			// off on; the failed write is reported rather than substituted.
			log.FromContext(ctx).Error(werr, "recording the group's status")
		}
	}
	return res, err
}

// proxyObservation is what one pass of reconcileObserved saw. observed is a
// flag and not a nil check on svc: HostPort creates no Service at all, so a
// nil svc is that strategy's normal state and cannot stand in for "this pass
// never looked".
type proxyObservation struct {
	observed bool
	pods     []corev1.Pod
	svc      *corev1.Service
}

// reconcileObserved does everything from the namespace bootstrap onward and
// returns what it saw, so that its caller can record the status however this
// returns. Its result carries the RequeueAfter the successful path has always
// returned; the caller adds nothing to it.
func (r *ProxyGroupReconciler) reconcileObserved(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
) (proxyObservation, ctrl.Result, error) {
	var obs proxyObservation

	if err := r.Bootstrap.Ensure(ctx, group.Namespace); err != nil {
		return obs, ctrl.Result{}, err
	}
	// Rendered here, beside Bootstrap.Ensure and before reconcileReplicas can
	// create the first proxy pod: that pod's projected volume names this
	// ConfigMap by group (podspec.GroupConfigMapName), and returning on error
	// stops the reconcile before reconcileReplicas runs — the guarantee is
	// the early return, not the order these lines happen to be written in.
	if err := r.reconcileConfigMap(ctx, group); err != nil {
		return obs, ctrl.Result{}, err
	}
	svc, err := r.reconcileService(ctx, group)
	if err != nil {
		return obs, ctrl.Result{}, err
	}
	// A snapshot from before this pass's own creates: reconcileReplicas may
	// create pods below, but pods itself is not refreshed to include them, so
	// its per-pod logic -- the readiness assertion loop and the divergence
	// check riding along with it -- sees a newly created pod for the first
	// time on the pass after this one, not this one.
	pods, err := r.pods(ctx, group)
	if err != nil {
		return obs, ctrl.Result{}, err
	}
	// From here the pass has seen both, so every return below carries an
	// observation the caller will record.
	obs = proxyObservation{observed: true, pods: pods, svc: svc}

	if err := r.reconcileReplicas(ctx, network, group, pods); err != nil {
		// A create the API server refused is the group's business and not
		// only the log's. IsForbidden covers both ways it happens: a Pod
		// Security profile that forbids the pod's shape -- which is how every
		// HostPort group in a baseline or restricted namespace ends -- and an
		// RBAC grant the operator does not have. IsInvalid covers a pod the
		// API server rejects outright, a quota or a webhook among them.
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			if setProxyPodsBlocked(group, spawneryv1alpha1.ReasonProxyPodRejected, err.Error()) {
				r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, "ProxyPodBlocked",
					actionCreateProxyPod, "%s",
					eventNote("the API server refused a proxy pod: %s", err.Error()))
			}
			// The phase is not set here and the status is not written here.
			// setStatus derives Degraded from the condition this branch just
			// set, and the caller writes on every path that got this far.
		}
		return obs, ctrl.Result{}, err
	}

	// Re-read after the changes, so the status describes what is there rather
	// than what was there when the reconcile started. A failure here leaves
	// the earlier snapshot in place: a status describing the last observation
	// that succeeded is a weaker statement than this pass meant to make and a
	// far stronger one than none.
	if pods, err = r.pods(ctx, group); err != nil {
		return obs, ctrl.Result{}, err
	}
	obs.pods = pods

	// Before setStatus: setStatus reads the Degraded condition to derive the
	// phase, so whether a pod this group asked for failed to come into
	// existence has to be known before that read.
	r.reportBlockedProxies(group, pods)
	// Before setStatus: the budget's selector has to find the label already on
	// the pods it is sizing minAvailable for, on the same pass.
	if err := r.protectOccupiedProxies(ctx, group, pods); err != nil {
		return obs, ctrl.Result{}, err
	}
	return obs, ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// refuse is the shared tail of the three paths that give up before
// reconcileReplicas: a missing Network, one that is not Accepted, and an
// expose.type this operator has no branch for. The third is unreachable
// while the CRD's enum and exposeImplemented agree — the API server rejects
// any object this operator could not classify — but the branch, and this
// tail, stay in place for the day they disagree. The caller has already put
// the reason on the group's Accepted condition; this does everything that is
// the same for all three.
//
// Divergence is forgotten because this return is before reconcileReplicas,
// and reportReadinessDivergence -- the only caller of Divergence.observe --
// runs nowhere else, so no observation of this group is coming on this pass.
// See the comment on the readinessDivergence type for why forgetting, not a
// TTL, is the right response to a gap in observation. protectPlayersOnly does
// call r.pods(), which observes the *expectations*: a different structure,
// with its own TTL, that this reasoning does not touch.
//
// The status is written whether or not protectPlayersOnly succeeded, and the
// reason is the one the accepted path states for its own early write: a
// reconcile that returned having recorded nothing leaves the object with no
// conditions and no phase, indistinguishable from one no reconcile has ever
// touched. A permanent failure in protectPlayersOnly -- a PodDisruptionBudget
// sitting at this group's name and owned by something else is a way to get
// one -- would otherwise hide the refusal that explains the group's whole
// state, on exactly the groups whose state most needs explaining.
//
// The requeue is networkRetryInterval rather than resyncInterval, which is
// what bounds how stale the budget can get here; see protectPlayersOnly.
// controller-runtime ignores it when the error is non-nil and backs off
// instead, which is the behaviour wanted for a failed protection pass.
func (r *ProxyGroupReconciler) refuse(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) (ctrl.Result, error) {
	r.Divergence.forget(group.Namespace + "/" + group.Name)
	protectErr := r.protectPlayersOnly(ctx, group)
	if err := r.writeStatus(ctx, group); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: networkRetryInterval}, protectErr
}

// protectOccupiedProxies is the pair that keeps the eviction API off a proxy
// with players on it: the podspec.LabelOccupied label, and the
// PodDisruptionBudget whose selector matches it.
//
// The two are one step and not two, and the order inside it is load-bearing:
// the budget's selector has to find the label already on the pods it is
// sizing minAvailable for, on the same pass. occupied is syncOccupiedLabels's
// own tally, taken while it is already looking at each pod to decide the
// label -- not a second count reconcileProxyPDB derives by asking the
// registry again. See the comment on proxyOccupied for why a second call is
// not safe here.
func (r *ProxyGroupReconciler) protectOccupiedProxies(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) error {
	occupied, err := r.syncOccupiedLabels(ctx, pods)
	if err != nil {
		return err
	}
	return r.reconcileProxyPDB(ctx, group, occupied)
}

// protectPlayersOnly is what this group still owes its players on the paths
// that give up before reconcileReplicas: a missing Network, one that has not
// been accepted, and an expose.type this milestone refuses.
//
// The rule is the one ServerGroupReconciler.Reconcile states at its own
// Network gate. A step that keeps the eviction API off an occupied pod, or
// that moves players off a node that is going away, does not depend on the
// Network — the players on these pods are already connected, and a group the
// operator has refused is still carrying them. Creating pods, sizing the
// group and publishing where to connect all genuinely need a usable Network
// and stay above these returns; the occupied label, the budget sized from it
// and the NodeDraining condition do not, and this is where they run instead.
//
// Without this, a ProxyGroup whose Network broke stopped maintaining its
// budget entirely. If its proxy was empty at the last successful pass the
// budget froze at minAvailable: 0 with no label on the pod, and a player
// joining afterwards was on a pod the eviction API could take with nothing
// standing in the way. That is a disconnect, not a hung drain.
//
// nodeDeparting is asked once per pod here, which is the only time it is
// asked on these paths -- reconcileReplicas, which computes the same fact for
// the rollout and hands it to reportNodeDraining, does not run on any path
// that reaches this function.
//
// The residual cost is timing, and it is worth stating: these paths requeue
// at networkRetryInterval rather than resyncInterval, so a proxy that becomes
// occupied while its group's Network is broken can wait up to that long to be
// counted, against five seconds on a healthy group. A bounded lag replaces an
// unbounded one.
func (r *ProxyGroupReconciler) protectPlayersOnly(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	pods, err := r.pods(ctx, group)
	if err != nil {
		return err
	}
	nodeGoing := make([]bool, len(pods))
	for i := range pods {
		nodeGoing[i] = nodeDeparting(ctx, r.Client, pods[i].Spec.NodeName, r.DrainTaintKeys)
	}
	r.reportNodeDraining(group, pods, nodeGoing)
	return r.protectOccupiedProxies(ctx, group, pods)
}

// pods lists the group's live proxy pods, oldest first, so scale-down is
// deterministic rather than map-order.
//
// The filter is podspec.ProxyLabels, the exact map reconcileService also uses
// as the Service selector — not a hand-written subset of it. Deriving both
// from the same function keeps them in agreement by construction rather than
// by the two places happening to match.
func (r *ProxyGroupReconciler) pods(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	labels := podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
	if err := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil, err
	}
	live := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if pod.DeletionTimestamp.IsZero() {
			live = append(live, pod)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].CreationTimestamp.Equal(&live[j].CreationTimestamp) {
			return live[i].Name < live[j].Name
		}
		return live[i].CreationTimestamp.Before(&live[j].CreationTimestamp)
	})
	// Namespace-qualified, matching every other key this reconciler's
	// Expectations uses (see reconcileReplicas and Reconcile's forget calls)
	// and the composite key ServerGroupReconciler keys its own Expectations
	// by -- a bare group.Name would collide across namespaces.
	r.Expectations.observePods(group.Namespace+"/"+group.Name, live)
	return live, nil
}

// proxyOccupied is the occupancy rule for a proxy pod, and the base of the
// only other one — proxyOccupiedForBudget below, which is this plus a single
// term. Whichever of the two a caller asks, it must ask once per pod per
// pass: syncOccupiedLabels evaluates its own exactly once and hands that one
// verdict to both the label it writes and the count reconcileProxyPDB sizes
// minAvailable from, rather than making two calls that would merely apply the
// same rule. A second call is not guaranteed to repeat the first: the
// registry these read from is mutated by live agent streams, and Lookup
// re-derives PlayersStale from the clock on every call, so two calls a few
// lines apart can disagree even with nothing running concurrently, purely
// because the clock moved between them.
//
// A proxy has no wasRegistered qualifier the way a server does — it sits behind
// the Service and players reach it directly — so for a running pod there is no
// state in which a stale count is safe to read as empty. Staleness alone is
// enough, and so is a stream that is down: Velocity goes on serving the
// sessions it holds after its agent's stream breaks, so a count nobody is
// updating says nothing about who is on it.
//
// This is the unqualified rule. It is read directly in one place — the
// deletion wait at the bottom of reconcileReplicas — and everywhere else
// through proxyOccupiedForBudget, which wraps it. The deletion wait can
// afford it unqualified because spec.drain.timeoutSeconds bounds that wait: a
// proxy this calls occupied forever is still deleted at its deadline. The
// label and the budget have no such bound, which is the whole of the
// difference between the two functions; see the wrapper for the term it adds
// and why the two consumers cannot share an answer.
func proxyOccupied(snap agent.Snapshot) bool {
	return snap.Players != 0 || snap.PlayersStale || !snap.Connected
}

// proxyOccupiedForBudget is proxyOccupied asked by the two consumers that have
// no deadline behind them: the podspec.LabelOccupied label, and the
// PodDisruptionBudget whose minAvailable is counted from it.
//
// The difference is one term, and the reason for it is that the two consumers
// are wrong in different directions. Registry.Lookup answers for a pod it has
// never seen with {Known: false, Connected: false, PlayersStale: true}, which
// proxyOccupied reads as occupied — so without this, every proxy pod counts
// and is labelled as occupied from the moment it appears until its agent
// connects and reports. On the deletion wait that costs a surplus pod its
// full drain deadline and no more. On a budget nothing puts a clock on it at
// all: a surge pod pushes minAvailable above the currentHealthy the group can
// reach and blocks every eviction in it until that pod's own agent finally
// reports, which for a proxy stuck in CrashLoopBackOff is never. The argument
// for dropping the server side's wasRegistered qualifier was written about
// the bounded consumer and does not carry to the unbounded one.
//
// snap.Known is the discriminator, and it is precise: Registry.Disconnect
// leaves it true, so a proxy whose agent connected and then died still counts
// as occupied — which is the case the conservative rule exists for, since
// Velocity goes on serving the sessions it holds after its agent's stream
// breaks. What is excluded is the pod the registry has never heard of at all
// — which, outside the window the next paragraph is entirely about, cannot be
// holding players: a proxy pod's readiness probe is served by its own agent
// (podspec.BuildProxyPod's TCP probe on ProxyReadyPort), so a pod whose agent
// has never come up is not Ready, is not an endpoint of the group's Service,
// and has no other route a player could arrive by — it publishes no hostPort
// and runs on no host network.
//
// Except in one window, which is what the grace is for. "Never seen" also
// describes every pod in the fleet in the moments after the operator itself
// restarts, and those pods are Ready and full of players: an operator evicted
// off the very node being drained is an ordinary way to reach that state, and
// kubectl drain will be retrying evictions throughout it. Registry.Lookup
// already answers this — for an unknown pod it reports StreamDownFor as the
// time since the operator started, which agent.Snapshot's own field comment
// describes as "a grace period to reconnect after an operator restart", and
// phase.StreamDownGrace is the length the server side already reads it at.
// Inside that window an unknown pod is treated as occupied, which is what
// protects the whole fleet while its agents dial back in. Outside it, an
// unknown pod is one whose agent has failed to appear for longer than the
// grace the server side already gives a stream that has gone quiet, and the
// group stops paying for it — deliberately, because that is the crash-looping
// pod the wedge above is about.
func proxyOccupiedForBudget(snap agent.Snapshot) bool {
	if !snap.Known && snap.StreamDownFor >= phase.StreamDownGrace {
		return false
	}
	return proxyOccupied(snap)
}

// syncOccupiedLabels keeps podspec.LabelOccupied on the group's pods in step
// with proxyOccupiedForBudget — not the bare proxyOccupied the deletion wait
// uses; see that pair of functions for the one term between them and why
// only the deadline-bounded consumer can afford the conservative answer — and
// returns how many pods it found occupied while doing
// so. reconcileProxyPDB sizes minAvailable from that returned count rather
// than asking the registry again itself — see the comment on proxyOccupied
// for why a second, independent call could answer differently and leave the
// label and the budget disagreeing pod for pod. A budget that counts fewer
// pods than carry the label hands the eviction API a disruption to spend on
// an occupied one; one that counts more blocks every eviction of a pod the
// selector matches — the occupied-labelled proxies, not an empty one, which
// this budget does not cover at all and which the eviction API can still
// take freely.
func (r *ProxyGroupReconciler) syncOccupiedLabels(ctx context.Context, pods []corev1.Pod) (int32, error) {
	var occupiedCount int32
	for i := range pods {
		pod := &pods[i]
		occupied := proxyOccupiedForBudget(r.Agents.Lookup(string(pod.UID)))
		if occupied {
			occupiedCount++
		}
		_, labelled := pod.Labels[podspec.LabelOccupied]
		// No write when nothing changed: this runs every five seconds per pod,
		// and a patch per pass would be a write per pod per pass for the life
		// of the group.
		if occupied == labelled {
			continue
		}
		patched := pod.DeepCopy()
		if occupied {
			if patched.Labels == nil {
				patched.Labels = map[string]string{}
			}
			patched.Labels[podspec.LabelOccupied] = "true"
		} else {
			delete(patched.Labels, podspec.LabelOccupied)
		}
		// NotFound tolerated for the same race markDraining's patch documents.
		// On the ordinary path this loop runs over a pods() read taken after
		// reconcileReplicas has already deleted this pass's newly-empty pods:
		// a pod that just went from occupied to empty and was deleted for it
		// still carries the occupied label in that read — occupied recomputes
		// false, labelled is still true, and the mismatch above builds a patch
		// for a pod the API server no longer has. protectPlayersOnly reaches
		// this loop without reconcileReplicas having run, so it cannot race
		// this pass's own delete; it can still race one from any other source,
		// which is the same tolerance and the same reason. Failing the whole
		// Reconcile over a label write for a pod that is already gone would
		// abort the rest of this pass for this group — its budget, its status
		// write, and every other pod's label still to come in this loop.
		if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return occupiedCount, nil
}

// reconcileProxyPDB keeps the group's PodDisruptionBudget in step with the
// number of occupied proxy pods. occupied is syncOccupiedLabels's own tally,
// not a second count this function derives on its own — see the comment on
// proxyOccupied for why a second registry read is not safe here.
//
// The same formulation as the ServerGroup's, for the same reason: for pods
// without a controller carrying a scale subresource, Kubernetes allows
// neither maxUnavailable nor percentages in a PDB. The absolute number of
// occupied pods is the only one that works, and it makes the eviction API
// refuse to evict any of them.
//
// Without this object, kubectl drain evicts a proxy in the same second the
// node is cordoned, while the replacement this operator ordered is still
// pulling its image -- and everyone on that proxy is disconnected by the
// eviction rather than carried by the drain.
//
// Named through podspec.GroupPDBName, not group.Name: a ServerGroup and a
// ProxyGroup can share a name in one namespace, and GroupPDBName's own doc
// comment narrates what naming this object after the bare group name would
// do about it.
func (r *ProxyGroupReconciler) reconcileProxyPDB(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
	occupied int32,
) error {
	minAvailable := intstr.FromInt32(occupied)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.GroupPDBName(group.Name, podspec.RoleProxy),
			Namespace: group.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.MinAvailable = &minAvailable
		pdb.Spec.MaxUnavailable = nil
		// Derived from ProxyLabels, the same function reconcileService uses as
		// the Service selector, plus the occupancy label -- not a hand-written
		// map that happens to match one. ProxyLabels returns a fresh map
		// literal on every call, so mutating it here cannot reach back into
		// the Service selector.
		selector := podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
		selector[podspec.LabelOccupied] = "true"
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		return controllerutil.SetControllerReference(group, pdb, r.Scheme)
	})
	return err
}

// reconcileReplicas creates or removes pods until the count matches the spec,
// and replaces pods that are stale: whose rendered shape no longer matches
// the group, or that sit on a node that is going away.
//
// Which pods go is DecideRollout's answer, not this function's: stale before
// current, then the fewest players, then the newest. That replaces a rule
// which took the newest first, full stop, because an older proxy has had
// longer to collect players. The old rule was a guess at the player count; the
// operator now has that count reported by the proxies' own agents, so it uses
// the number where it has one and falls back to the guess — unchanged, and
// still newest first — where it does not. Staleness comes first because a
// stale pod has to go regardless, and taking a current one ahead of it would
// drain two pods for one replacement.
func (r *ProxyGroupReconciler) reconcileReplicas(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) error {
	// The two places whose failure leaves nothing hashed or judged: without
	// configValues there is nothing to hash, and without the digest no pod's
	// staleness can be judged, so continuing past either would either roll
	// nothing or roll everything.
	configValues, err := yaml.Marshal(proxyConfigValues(group))
	if err != nil {
		return err
	}
	wantHash, err := podspec.DesiredProxyHash(network, group, r.AgentEndpoint, configValues)
	if err != nil {
		return err
	}

	// nodeGoing is this pass's nodeDeparting verdict for each live pod, kept
	// alongside views by the same index so it can be reused below (the
	// draining mark's event gate, and the NodeDraining condition) instead of
	// being asked a second time. Task 8's brief names the hazard directly:
	// the same fact computed twice from the same source at two different
	// moments can disagree.
	nodeGoing := make([]bool, len(pods))
	views := make([]ProxyView, 0, len(pods))
	for i := range pods {
		snap := r.Agents.Lookup(string(pods[i].UID))
		_, dated := drainingSince(&pods[i])
		nodeGoing[i] = nodeDeparting(ctx, r.Client, pods[i].Spec.NodeName, r.DrainTaintKeys)
		views = append(views, ProxyView{
			Name: pods[i].Name,
			// Two ways to be out of date, and the rollout does not distinguish
			// them: a pod whose rendered shape no longer matches the group, and
			// a pod on a node that is going away. Both have to be replaced by a
			// pod somewhere else, one at a time, without disconnecting anyone —
			// which is the sentence DecideRollout already implements.
			Stale:        pods[i].Labels[podspec.LabelPodHash] != wantHash || nodeGoing[i],
			Ready:        isPodReady(&pods[i]),
			Draining:     dated,
			Players:      snap.Players,
			PlayersStale: snap.PlayersStale,
			CreatedAt:    pods[i].CreationTimestamp.Time,
		})
	}
	// The group's NodeDraining condition is built from this pass's live set,
	// independent of whether DecideRollout has actually picked any of these
	// pods to drain yet -- it reports where pods are, not what has been
	// decided about them. No event accompanies it: the per-proxy event fired
	// below, at the point a proxy is actually marked, is the one §3.7 asks
	// for, and a second event tied to this condition's own transition would
	// under-report a group where a second departing node appears while the
	// condition is already True.
	r.reportNodeDraining(group, pods, nodeGoing)

	decision := DecideRollout(views, group.Spec.Replicas)

	// DecideRollout sizes target - total from views, which pods() has already
	// read through the manager's cached client: a reconcile triggered by its
	// own create can run again before that create is visible, see the group
	// still short by the same count, and decide to create it a second time.
	// NewProxyName is NewServerName by direct delegation, so it draws the same
	// fresh crypto/rand suffix on every call: the race described above is not
	// a retry of the name already in flight -- apierrors.IsAlreadyExists below
	// has nothing to catch, because the second create asks for a different,
	// genuinely distinct pod for the slot the first one already fills. The
	// real asymmetry with ServerGroupReconciler.createServer is elsewhere: it
	// treats any Create error as fatal, where the loop below tolerates
	// AlreadyExists -- a difference in error handling, not in how the two
	// names are generated. key is namespace-qualified: see the comment on
	// pods() for why.
	//
	// The correction is arithmetic, not a bare gate: capping at zero the
	// moment anything is pending would also block a create the group still
	// legitimately needs beyond the one already reserved. Subtracting exactly
	// what is reserved is what ServerGroupReconciler.size does through
	// DecideSize's alive := in.PendingCreates (scaling.go); this states the
	// same correction as a post-processing step on DecideRollout's answer
	// instead, so rollout.go's sizing keeps knowing nothing about reservations.
	key := group.Namespace + "/" + group.Name
	pendingCreates, _, _ := r.Expectations.pending(key)
	create := decision.Create - int32(len(pendingCreates))
	if create < 0 {
		create = 0
	}
	// A failure partway through this loop -- either return below -- leaves the
	// reservations made by whichever earlier iterations already succeeded
	// standing past this Reconcile: Reconcile's second pods() call, the one
	// that would otherwise observe and clear them, never runs on an error
	// return. That is deliberate, not a leak: the pods those reservations
	// name were actually created, controller-runtime requeues on the
	// returned error, and it is exactly this surviving reservation that stops
	// the retried reconcile -- which may run before a real informer cache has
	// caught up with those creates -- from creating duplicates for slots the
	// failed pass already filled. It clears on the next pass's own pods()
	// call once the cache shows it, or on the TTL if that call is somehow
	// never reached.
	for i := int32(0); i < create; i++ {
		pod, err := podspec.BuildProxyPod(network, group, NewProxyName(group.Name), r.AgentEndpoint, configValues)
		if err != nil {
			return err
		}
		if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		// Reserved on the same terms the loop above already tolerates: an
		// AlreadyExists means a pod under this name exists in the API server
		// either way, so it is as reserved as one this call just created.
		// Reserved only here, matching ServerGroupReconciler.size, which calls
		// expectCreated after createServer returns successfully and not
		// before: a failed Create returns from this function immediately,
		// and no reservation is made for a create that never happened. There
		// is nothing to expire on the TTL in that case, because nothing was
		// recorded.
		r.Expectations.expectCreated(key, pod.Name)
	}

	// Which pods are going, decided once and used by both loops below.
	//
	// Derived rather than re-read from the annotation: on the pass that first
	// marks a pod, the annotation is not on it yet when the readiness loop
	// runs, and readiness would lag a whole pass behind the mark.
	leaving := make(map[string]bool, len(decision.Drain))
	for _, name := range decision.Drain {
		leaving[name] = true
	}
	// A pod already carrying the mark keeps it while the group still wants it
	// gone, because DecideRollout deliberately names nobody while another pod
	// is draining — that is what makes the update one proxy at a time. Without
	// this the drain started last pass would be cancelled on the next one and
	// made again on the one after, and each cancellation deletes the
	// annotation, so the deadline would start from zero every time.
	//
	// Wanting it gone is two different questions, and they have to be asked
	// differently. A stale pod has to go whatever else is true of the group, so
	// that mark is kept per pod. Being surplus is not a property of any pod at
	// all: it is a shortfall of one number against another, and no pod can be
	// asked about it. Asking "is the group over its count?" once per marked pod
	// gives the same answer for every one of them, so a group that lowered
	// replicas twice and then raised them partway keeps every mark it made
	// rather than the number it still needs, and sits under capacity until a
	// drain finishes on its own. Nothing rescues it in the meantime: the group
	// is not short of pods, only of ready ones, so DecideRollout's create
	// branch does not fire, and its one-at-a-time gate returns before it could
	// decide anything else.
	//
	// The number it needs comes out of what must be left standing:
	//
	//	len(views) - staleMarks - keptSurplusMarks >= replicas
	//
	// so at most len(views)-staleMarks-replicas surplus marks are kept. A pod
	// already leaving for being stale must not also spend the surplus budget,
	// or the group holds a mark for a pod it needs back and counts the same
	// departure twice.
	//
	// The term subtracts stale *marks* rather than stale pods because what the
	// invariant is about is pods that are leaving, and a stale pod with no mark
	// is still serving — it will be marked on a later pass, one at a time.
	//
	// The two counts come apart in one state, and it is a state this milestone
	// exists to serve: a spec change reverted while a proxy is draining for it.
	// Take a group of two on v1, change the image, wait for the surge pod, and
	// let one old proxy be marked; then put the image back. The marked pod
	// matches the spec again and the surge pod does not, so a pod that was a
	// stale mark is now a surplus mark and the pod nobody has marked is the
	// stale one. Subtracting stale pods would release the mark on the spot,
	// because the surge pod's eventual departure is counted as if it had
	// already happened; subtracting stale marks holds it, and the surge pod is
	// marked on a later pass when this one has finished.
	//
	// Holding it is the conservative reading and the one that shipped: the
	// budget is spent only on departures that are actually under way, so a spec
	// that flaps does not release a drain it will want back. It costs the group
	// one proxy more than the minimum, drained one at a time and bounded by the
	// deadline like every other wait here.
	// TestARevertedSpecChangeKeepsTheMarkItAlreadyMade pins it, so this is a
	// decision the code states rather than one a comment claims.
	//
	// The count is what keeps a drain cancellable. Persisting every mark would
	// make a marked pod's readiness remembered rather than derived, and a
	// scale-down reversed before its pod was gone would not be reversed on the
	// pod: it would sit NotReady until it emptied, and then be replaced by one
	// of the same shape.
	var staleMarks int32
	surplusMarks := make([]ProxyView, 0, len(views))
	for _, v := range views {
		switch {
		case !v.Draining:
		case v.Stale:
			leaving[v.Name] = true
			staleMarks++
		default:
			surplusMarks = append(surplusMarks, v)
		}
	}
	// pick, so that the marks released are the ones a fresh decision would not
	// have made: a pod the kubelet still calls Ready goes back into service
	// ahead of anything else, then the least trusted, then the fullest — and
	// the pods that keep their marks are the ones the same rule would choose
	// again. That last clause is the load-bearing one and it holds by
	// construction, because a fresh DecideRollout sorts with this same
	// comparator.
	//
	// Readiness leads the enumeration because the clause added for the
	// unready-stale stall sits above both player clauses, and it reads well
	// here for a reason of this loop's own: a draining pod the kubelet still
	// calls Ready is one whose withdrawal has not taken effect yet, so it is
	// the mark that has made the least progress and the cheapest to give back.
	//
	// Ordering cannot make this oscillate, and the reason is stronger than
	// pick being deterministic: a released pod loses its annotation on the same
	// pass, so it is not Draining on the next one and drops out of this
	// candidate set. Player counts moving underneath can therefore change which
	// of the marks still standing would be kept, but nothing here can hand a
	// mark back to a pod this loop released — only a fresh decision can, and
	// DecideRollout makes none while anything is draining.
	//
	// The set is not monotone. It grows whenever a pass with nothing draining
	// decides a surplus, which is how every mark here first appears; the
	// exception while a drain is under way is the rollback above, where a mark
	// held in the stale branch moves into this one when the spec comes back.
	// Both add a candidate rather than restoring a released one, so neither can
	// start the cycle this paragraph rules out.
	// One direction here is open and is left open knowingly, so that the next
	// reader has the question rather than having to find it. views is built
	// from the pods this pass could see, so a create the informer cache has
	// not caught up with makes len(views) one low, which makes want one low,
	// which releases one more mark than a correct count would. Releasing a
	// mark deletes the annotation, so that pod's deadline restarts from zero
	// when the group decides it should go after all -- a drain that had nearly
	// run its course begins again, and the players on it wait another full
	// timeout.
	//
	// Whether that is reachable was not settled. Four candidate states were
	// traced -- scale-down then up with a create pending, a rollback with the
	// surge pod lost, replicas raised mid-rollout, and a mixed-generation
	// surplus -- and in each either surplusMarks was empty or want did not
	// cross 1 to 0, so none of them reaches it. That is not a proof it is
	// unreachable. Note the direction is the opposite of the create cap's
	// above, where the same undercount is conservative; here it is not, which
	// is the reason to write it down.
	if want := int32(len(views)) - staleMarks - group.Spec.Replicas; want > 0 {
		for _, name := range pick(surplusMarks, want) {
			leaving[name] = true
		}
	}

	// The desired readiness is derived, not remembered: this loop already
	// knows which pods are surplus, so it asserts the answer for every pod on
	// every pass. An operator restart recomputes the same thing, and a
	// cancelled scale-down corrects itself without anything to clean up —
	// including one cancelled part of the way, which returns as many proxies to
	// service as the new count asks for and leaves the rest draining.
	// diverging and names feed reportReadinessDivergence below: it needs to
	// know, for every live pod, both what was just asserted for it and what
	// the kubelet actually reports, and this loop is the only place both are
	// in hand at once -- going is local to this function, and isPodReady
	// reads the same pod this loop already has open.
	//
	// Only the withdrawal direction counts: going && isPodReady is true
	// exactly when a pod was just told to stop taking connections and the
	// kubelet still calls it Ready -- an agent that heard the instruction
	// and did not act on it, which is what this condition exists to catch.
	// The reverse -- asserted ready but not yet actually Ready -- is
	// deliberately not checked. SetReady(true) is asserted for a
	// non-draining pod from the moment it exists, before any kubelet has
	// had a chance to probe it even once, and a cold image pull can easily
	// outrun readinessDivergenceGrace without the pod having disobeyed
	// anything: it is starting up, not diverging, and reporting that
	// direction would misname it as an agent that heard an instruction and
	// ignored it. A proxy that is supposed to be ready and never gets there
	// is already visible without this: the group sits below its ready
	// count, and known-issues.md's "a proxy that cannot bind its ready port
	// is silent on the CR" entry (filed under 3c, the milestone that added
	// the bind) is the diagnosis that direction actually needs.
	diverging := make(map[types.UID]bool, len(pods))
	names := make(map[types.UID]string, len(pods))
	for i := range pods {
		going := leaving[pods[i].Name]
		// Read before markDraining below can change it: views[i].Draining is
		// this same pod's drainingSince verdict from the loop that built
		// views, taken before anything in this pass patched its annotations,
		// so it is what "already marked, before this call" means for the
		// event gate two lines down.
		wasMarked := views[i].Draining
		if err := r.Proxies.SetReady(ctx, string(pods[i].UID), !going); err != nil {
			return err
		}
		if err := r.markDraining(ctx, &pods[i], going); err != nil {
			return err
		}
		// The event §3.7 asks for on the ProxyGroup: fired once, on the pass
		// that actually marks a proxy, and only when the node is a reason it
		// was marked -- not when a hash mismatch is the only reason. going is
		// leaving[pods[i].Name] and can be true for reasons nodeGoing[i] does
		// not cover (a plain surplus reduction, a hash mismatch alone), so
		// nodeGoing[i] is what keeps this from firing on those. !wasMarked is
		// what keeps it from firing again on every later pass the same pod
		// spends draining, matching markDraining's own guard against
		// re-stamping a mark it already made.
		if going && !wasMarked && nodeGoing[i] {
			r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, spawneryv1alpha1.ReasonNodeDraining, actionDrainProxy,
				"draining proxy %s off a node that is going away", pods[i].Name)
		}
		diverging[pods[i].UID] = going && isPodReady(&pods[i])
		names[pods[i].UID] = pods[i].Name
	}
	r.reportReadinessDivergence(group, key, diverging, names)

	// Removal waits for the pod to be empty. Readiness stopped the inflow —
	// the Service dropped the endpoint, so no new connection arrives — but it
	// said nothing about the players already on it, whose TCP sessions
	// Kubernetes does not close. Deleting at NotReady would disconnect exactly
	// the people the readiness contract exists to protect.
	//
	// Nobody is moved, and that is not an omission. A draining server can hand
	// its players to another backend because the client's connection
	// terminates at the proxy, which stays; a draining proxy has no such
	// option, because the connection terminates at the proxy being removed.
	// So the deadline below is the only path here that disconnects anyone.
	//
	// Empty means the count is fresh, zero, and reported by a stream that is
	// still up. A count we cannot trust is treated as occupied — proxyOccupied
	// above states it for a proxy pod, on the same principle candidates.go's
	// isOccupied states for the Server controller. It matters more here than the
	// phrasing suggests: an agent's gRPC stream breaking does not disconnect
	// anybody, because Velocity goes on serving the sessions it already holds,
	// and the registry then reports a pod it has never heard of and a pod
	// whose agent died three minutes ago identically — both as zero players.
	// Deleting on a bare zero would disconnect everyone on a proxy whose only
	// fault was a dropped stream, which is precisely the failure this wait
	// exists to prevent.
	//
	// Connected is what makes "three minutes ago" and "three seconds ago" the
	// same answer. Registry.Disconnect deliberately keeps the last count and
	// leaves lastReportAt alone, so freshness alone still believes a zero for
	// 2 × the report interval — 10 s at the operator's configured 5 s — after
	// the stream that produced it died. A player can join through the Service
	// inside that window, because the pod is Ready until its own agent closes
	// the gate, and Velocity accepts them with nothing left to report it. So
	// the count is only believed while the stream that would have updated it
	// is still there.
	//
	// isOccupied's own rule also requires wasRegistered, because nobody is
	// ever routed to a server the proxies were not told about. A proxy has no
	// such qualifier: it sits behind the Service and players reach it
	// directly, so for a pod that is still running there is no state in which
	// a stale count is safe to read as empty. Staleness alone is enough, and
	// the cost is bounded — a proxy whose agent never appears is removed by
	// the deadline rather than never.
	//
	// That bound is why this call is proxyOccupied and the label and budget
	// use proxyOccupiedForBudget instead. The two ask the same question of
	// the same snapshot; only this one has a deadline underneath it, so only
	// this one can afford to be wrong in the conservative direction with
	// nothing to end it. A budget has no such deadline: a minAvailable held
	// above an achievable currentHealthy lifts when the pod's agent finally
	// reports and not before, so the same conservative answer applied there
	// blocks every eviction in the group for as long as that takes — for a
	// pod whose agent never arrives, for as long as the pod exists. Same
	// question, different consequence for being wrong, so the two consumers
	// do not share an answer.
	//
	// isOccupied has a third term this deliberately does not model:
	// sessionsGone, which overrides even a non-zero count, because a pod that
	// reached a terminal state took its sessions down with it. A crashed
	// surplus proxy therefore waits out its full deadline here rather than
	// going immediately, and is then announced as losing players who were
	// disconnected by the crash. That is wasteful and the event overstates,
	// but both err towards keeping a pod that might still have someone on it,
	// and the wait is bounded. Reading pod state to decide it is a wider
	// change than this task, which only ever asks the registry.
	for i := range pods {
		if !leaving[pods[i].Name] {
			continue
		}
		pod := &pods[i]
		snap := r.Agents.Lookup(string(pod.UID))
		players := snap.Players
		// A pod with no readable stamp has no deadline. It is not a dead end:
		// the assertion loop above has already re-stamped every surplus pod
		// that lacked one, this pod included, so the deadline starts on this
		// pass rather than never.
		since, dated := drainingSince(pod)
		expired := dated && r.Clock().Sub(since) >= group.DrainTimeout()

		switch {
		case !proxyOccupied(snap):
			// Known empty: nobody is on it, so removing it costs nothing.
		case expired:
			// The one path in this milestone that disconnects anybody. It is
			// configured rather than accidental, so it says so loudly and
			// names the cost.
			r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, "ProxyDrainTimeout", actionDrainProxy,
				"deleting proxy %s after %s with %d player(s) still connected",
				pod.Name, group.DrainTimeout(), players)
		default:
			// Still draining. Nothing to do; the next pass looks again.
			continue
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		// Reserved after the Delete succeeds (or the pod was already gone),
		// matching ServerGroupReconciler.size's expectDeleted -- and, like the
		// create loop above, matching what it does on failure too: an error
		// other than NotFound returns above before this line runs, so nothing
		// is reserved for a delete that did not go through.
		r.Expectations.expectDeleted(key, pod.Name)
	}
	return nil
}

// reportReadinessDivergence tells the group's ReadinessDiverged condition
// what r.Divergence has observed for this pass's pods, and fires the one
// event this readiness contract needs beyond Resync's own repeated
// assertion: a pod that was told to stop taking connections and the kubelet
// still calls Ready, for at least readinessDivergenceGrace. A divergence
// caused by a lost SetReady or a lost pod-status update heals on the next
// resync; what survives that is an agent that received the withdrawal and
// did not act on it, and reporting -- not repairing -- is what this
// function does about it. See the comment on diverging in
// reconcileReplicas for why only this direction is checked.
//
// Built false-by-default and flipped, like ScalingLimited and BackingOff in
// ServerGroupReconciler.Reconcile. The event goes on the flank only, for the
// same reason theirs does: SetStatusCondition moves lastTransitionTime just
// on a change of status, so comparing IsStatusConditionTrue across the call
// is what tells a transition from a resync repeating the same verdict.
func (r *ProxyGroupReconciler) reportReadinessDivergence(
	group *spawneryv1alpha1.ProxyGroup,
	key string,
	diverging map[types.UID]bool,
	names map[types.UID]string,
) {
	stale := r.Divergence.observe(key, diverging, readinessDivergenceGrace)

	diverged := metav1.Condition{
		Type:    spawneryv1alpha1.ConditionReadinessDiverged,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonReadinessAgrees,
		Message: "no proxy pod has stayed Ready past its withdrawal for longer than the grace period",
	}
	if len(stale) > 0 {
		staleNames := make([]string, 0, len(stale))
		for _, uid := range stale {
			staleNames = append(staleNames, names[uid])
		}
		sort.Strings(staleNames)
		diverged.Status = metav1.ConditionTrue
		diverged.Reason = spawneryv1alpha1.ReasonReadinessDiverged
		diverged.Message = fmt.Sprintf(
			"%s still Ready %s after the operator withdrew its readiness; "+
				"the operator does not retry this, only reports it",
			strings.Join(staleNames, ", "), readinessDivergenceGrace)
	}

	was := meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged)
	meta.SetStatusCondition(&group.Status.Conditions, diverged)
	if isTrue := diverged.Status == metav1.ConditionTrue; isTrue != was {
		eventType := corev1.EventTypeNormal
		if isTrue {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(group, nil, eventType, diverged.Reason, actionSyncStatus, "%s",
			eventNote("%s", diverged.Message))
	}
}

// reportNodeDraining tells the group's NodeDraining condition which nodes,
// among this pass's live pods, are on their way out of service. nodeGoing is
// its caller's own per-pod nodeDeparting verdict -- never asked a second time
// here -- indexed the same way as pods.
//
// Two callers, one per pass and never both: reconcileReplicas hands over the
// verdict from the loop that built views, and protectPlayersOnly computes it
// itself on the paths where reconcileReplicas does not run at all. Both keep
// the fact to one evaluation per pass, which is what the shared parameter is
// for.
//
// No event goes with this: the one §3.7 asks for is fired in reconcileReplicas
// at the point a proxy is actually marked, which is a per-proxy occasion this
// condition's own True/False flank cannot stand in for -- a second departing
// node while the condition is already True marks another proxy without the
// condition changing at all.
func (r *ProxyGroupReconciler) reportNodeDraining(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, nodeGoing []bool) {
	names := make([]string, 0, len(pods))
	for i, going := range nodeGoing {
		if going {
			names = append(names, pods[i].Spec.NodeName)
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions, drainingCondition(names))
}

// drainingSince reads the annotation markDraining writes. ok is false when
// there is no usable start for the deadline to run from, which is two cases
// that behave identically from here: no annotation at all — the pod has not
// been asked to drain yet — and an annotation that does not parse.
//
// An unparsable stamp is deliberately not an error. The annotation is on a
// pod, so anybody who can write a pod annotation can produce one, and
// returning an error would abort the whole Reconcile — Service, status, and
// every other pod's readiness assertion with it — on every pass, forever,
// because nothing downstream of the error would ever rewrite the value. Every
// other API call reconcileReplicas makes tolerates one pod's bad state
// (Create tolerates AlreadyExists, Delete tolerates NotFound, markDraining's
// patch tolerates NotFound, Fleet.SetReady is a no-op for an unknown pod);
// this is the one whose input a user can write.
//
// Treating it as no stamp is what makes it recoverable: markDraining's guard
// keys on this function, so a surplus pod whose stamp does not parse is
// re-stamped with the current time on the same pass. See markDraining for
// what that costs.
func drainingSince(pod *corev1.Pod) (time.Time, bool) {
	raw, ok := pod.Annotations[ProxyDrainingSinceAnnotation]
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// markDraining writes or removes the annotation that dates a proxy's drain.
//
// Written once and never moved while it stays readable: the deadline runs
// from the first assertion, and re-stamping it on every five-second pass would
// push it forever and the drain would never end. So the write branch is keyed
// on drainingSince — on a stamp the deadline can actually run from — and not
// on the annotation merely being present.
//
// The one case where a stamp is rewritten is a stamp that does not parse,
// which is the only way out of it: drainingSince cannot read it and no other
// code path touches the annotation, so a guard keyed on presence would leave
// a corrupt value there for the pod's whole life. Rewriting costs that pod a
// restarted clock — its deadline runs from now rather than from whenever the
// drain really began, so it can wait up to one full spec.drain.timeoutSeconds
// longer than it should. That is the same direction every other rule here
// errs in: waiting too long keeps players connected, and the wait stays
// bounded. Nothing is lost that was ever readable.
//
// The removal branch keys on presence instead, so that cancelling a
// scale-down clears an unparsable stamp as well as a good one rather than
// leaving litter on a pod that is no longer draining.
//
// The patch tolerates NotFound like every other API call reconcileReplicas
// makes for a racing pod (Create tolerates AlreadyExists, Delete tolerates
// NotFound, Fleet.SetReady is a no-op for a pod it has no session for): a
// pod evicted or deleted between the informer list and this patch should
// not fail the whole reconcile over a stamp that no longer matters.
func (r *ProxyGroupReconciler) markDraining(ctx context.Context, pod *corev1.Pod, draining bool) error {
	raw, marked := pod.Annotations[ProxyDrainingSinceAnnotation]
	_, dated := drainingSince(pod)
	switch {
	case draining && !dated:
		if marked {
			log.FromContext(ctx).Info("replacing an unparsable draining-since; this proxy's drain deadline restarts from now",
				"pod", pod.Name, "namespace", pod.Namespace,
				"annotation", ProxyDrainingSinceAnnotation, "value", raw)
		}
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[ProxyDrainingSinceAnnotation] = r.Clock().UTC().Format(time.RFC3339)
		return client.IgnoreNotFound(r.Patch(ctx, pod, patch))
	case !draining && marked:
		patch := client.MergeFrom(pod.DeepCopy())
		delete(pod.Annotations, ProxyDrainingSinceAnnotation)
		return client.IgnoreNotFound(r.Patch(ctx, pod, patch))
	}
	return nil
}

// reconcileService keeps the group's Service in step with its expose
// strategy, and returns the Service it settled on so the caller can read
// status.loadBalancer off it without a second Get.
//
// HostPort gets no Service and returns nil: nothing inside the cluster dials
// a proxy. Players arrive from outside, agents dial the operator, and
// Velocity dials backends.
func (r *ProxyGroupReconciler) reconcileService(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
) (*corev1.Service, error) {
	if group.Spec.Expose.Type == spawneryv1alpha1.ExposeHostPort {
		return nil, r.deleteServiceIfOurs(ctx, group)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: group.Name, Namespace: group.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue

		// Unconditional rather than inside the LoadBalancer arm: a group that
		// leaves LoadBalancer has to release the keys it set, and nil is how
		// that is said.
		var lbAnnotations map[string]string
		if group.Spec.Expose.Type == spawneryv1alpha1.ExposeLoadBalancer &&
			group.Spec.Expose.LoadBalancer != nil {
			lbAnnotations = group.Spec.Expose.LoadBalancer.Annotations
		}
		applyExposeAnnotations(svc, lbAnnotations)

		// The selector must pin the role as well as the group: without it the
		// Service would also select any server pod that happened to share the
		// group name, and players would land on a backend directly.
		svc.Spec.Selector = podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)

		port := corev1.ServicePort{
			Name:       podspec.MinecraftPortName,
			Port:       podspec.MinecraftPort,
			TargetPort: intstr.FromString(podspec.MinecraftPortName),
			Protocol:   corev1.ProtocolTCP,
		}
		switch group.Spec.Expose.Type {
		case spawneryv1alpha1.ExposeLoadBalancer:
			svc.Spec.Type = corev1.ServiceTypeLoadBalancer
			svc.Spec.ExternalTrafficPolicy = loadBalancerTrafficPolicy(group)
			// No node port is named. A LoadBalancer Service gets one anyway,
			// allocated by the API server, and naming one here would add a
			// second way for two groups in different namespaces to collide
			// over a number no player ever dials.
		case spawneryv1alpha1.ExposeClusterIP:
			// No external traffic policy: the field is meaningless on a
			// ClusterIP Service and the API server rejects it. No node port
			// either, which is the whole reason this strategy exists -- the
			// NodePort workaround it replaces left one allocated that nobody
			// dialled and a host firewall had to account for.
			svc.Spec.Type = corev1.ServiceTypeClusterIP
		case spawneryv1alpha1.ExposeNodePort:
			// Local, not the Cluster default, for the same reason
			// LoadBalancerSpec.ExternalTrafficPolicy defaults to Local: the
			// default SNATs, so Velocity would never see a player's real IP,
			// and bans and rate limits depend on it. The consequence is the
			// trade-off this makes: a client that reaches a node running no
			// proxy pod for this group gets no answer at all, rather than
			// being routed to one that does. That is consistent with
			// proxyAddress only ever publishing the address of a node that
			// demonstrably runs a ready proxy -- a client dialing the
			// published address never hits the empty case.
			svc.Spec.Type = corev1.ServiceTypeNodePort
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			port.NodePort = group.Spec.Expose.NodePort.Port
		default:
			// Unreachable while exposeImplemented and this switch agree, and
			// written out because that is exactly the assumption a fifth
			// strategy breaks: whoever adds one and updates only one of the
			// two gets a named error here instead of the nil dereference the
			// old default: arm produced by reading NodePort.Port.
			return fmt.Errorf("expose.type %s reached reconcileService without a branch",
				group.Spec.Expose.Type)
		}
		svc.Spec.Ports = []corev1.ServicePort{port}

		return controllerutil.SetControllerReference(group, svc, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return svc, nil
}

// applyExposeAnnotations reconciles the annotations the operator owns on a
// Service, leaving every other key untouched. See
// podspec.AnnotationExposeAnnotations for why the record is necessary.
func applyExposeAnnotations(svc *corev1.Service, want map[string]string) {
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	if owned := svc.Annotations[podspec.AnnotationExposeAnnotations]; owned != "" {
		for _, k := range strings.Split(owned, ",") {
			if _, still := want[k]; !still {
				delete(svc.Annotations, k)
			}
		}
	}
	keys := make([]string, 0, len(want))
	for k, v := range want {
		svc.Annotations[k] = v
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		delete(svc.Annotations, podspec.AnnotationExposeAnnotations)
		return
	}
	sort.Strings(keys)
	svc.Annotations[podspec.AnnotationExposeAnnotations] = strings.Join(keys, ",")
}

// loadBalancerTrafficPolicy is the CRD's Local default, restated in code.
// A ProxyGroup built in a unit test never passes through the API server's
// defaulting, and an empty policy on a Service is not a valid value -- the
// same hazard podspec.DefaultDrainTimeoutSeconds exists for.
func loadBalancerTrafficPolicy(group *spawneryv1alpha1.ProxyGroup) corev1.ServiceExternalTrafficPolicy {
	if lb := group.Spec.Expose.LoadBalancer; lb != nil && lb.ExternalTrafficPolicy != "" {
		return lb.ExternalTrafficPolicy
	}
	return corev1.ServiceExternalTrafficPolicyLocal
}

// deleteServiceIfOurs removes the Service a group had before it switched to
// HostPort, and only that one.
//
// The ownership check is the whole of the function's care: a Service
// somebody else put at the group's name is not this operator's to remove,
// and a delete is the one action here that cannot be undone. The
// preconditions pin the object the decision was made about -- between the
// Get and the Delete the name could have come to hold a different object
// entirely.
//
// Both halves of that check are required, as design section 4 specifies: a
// controller reference whose UID is this group's, and podspec.LabelManagedBy.
// The UID is the strong half -- it fails for a Service controlled by anything
// else, this group's own predecessor of the same name included, which is the
// case the garbage collector can leave standing after a delete-and-recreate.
// The label is the cheap half and is kept anyway, because it is the same
// label cmd/spawnery-operator narrows the manager's cache by for every other
// kind the operator writes. Services are the one kind it does not narrow
// (main.go's ByObject list covers ConfigMaps, ServiceAccounts and PVCs), so
// this function is the only thing standing between a stray object at the
// group's name and an irreversible delete, and it checks everything the
// operator stamps rather than the cheapest sufficient thing.
func (r *ProxyGroupReconciler) deleteServiceIfOurs(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: group.Name}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner := metav1.GetControllerOf(svc); owner == nil || owner.UID != group.UID {
		return nil
	}
	if svc.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		return nil
	}
	uid := svc.UID
	rv := svc.ResourceVersion
	return client.IgnoreNotFound(r.Delete(ctx, svc, &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
	}))
}

// reconcileConfigMap keeps the group's rendered ConfigMap — design section
// 5.4's one ConfigMap per group — in step with the fields spec.config exposes
// to a user. It carries playerLimit, motd and onlineMode: the forwarding mode
// and the ports are operationally critical, and live in internal/render's
// critical layer and nowhere else, so there is exactly one place that can be
// wrong about either. onlineMode is written here too, and is still critical in
// the sense that matters — no configOverlay can reach it — but its value is a
// deliberate choice a user makes on the ProxyGroup, so it travels as a value
// rather than as a constant in the renderer.
//
// It marshals a render.Values document under podspec.ConfigValuesKey, the
// same key BuildProxyPod projects into ConfigDir, and it carries
// podspec.LabelManagedBy for the reason podspec.GroupConfigMapName and
// Bootstrapper.ensureConfigMap both document: cmd/spawnery-operator narrows
// the manager's cache for ConfigMaps to that label, so an unlabelled one this
// reconciler just wrote would be invisible to it on the very next Get.
func (r *ProxyGroupReconciler) reconcileConfigMap(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	data, err := yaml.Marshal(proxyConfigValues(group))
	if err != nil {
		return fmt.Errorf("marshal config.yaml for group %s: %w", group.Name, err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.GroupConfigMapName(group.Name, podspec.RoleProxy),
			Namespace: group.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[podspec.ConfigValuesKey] = string(data)
		return controllerutil.SetControllerReference(group, cm, r.Scheme)
	})
	return err
}

// proxyConfigValues builds the neutral document reconcileConfigMap writes
// from whatever a user set in spec.config. PlayerLimit is never left nil —
// spec.config itself being nil counts as PlayerLimit unset, and it defaults
// to podspec.DefaultPlayerLimit, the exact constant BuildProxyPod defaults
// SPAWNERY_PLAYER_LIMIT from. The two must agree: if this function left the
// field nil instead, render.Velocity's RequirePlayerLimit would refuse every
// ProxyGroup that never set spec.config.playerLimit, while that same
// ProxyGroup's pods already claim a limit of 500 in their own environment —
// Accepted, Service up, and every pod in CrashLoopBackOff forever, with
// nothing on the CR saying why.
// OnlineMode is never left nil for the same reason and with a sharper edge:
// render.Velocity's RequireOnlineMode refuses to guess, and the value decides
// whether the proxy authenticates players at all. The default here is true —
// the same default the CRD stamps on spec.config.onlineMode — so a ProxyGroup
// whose spec.config is nil, or one created before the field existed and never
// defaulted, renders an authenticating proxy rather than an open one.
func proxyConfigValues(group *spawneryv1alpha1.ProxyGroup) render.Values {
	var values render.Values
	limit := podspec.DefaultPlayerLimit
	if cfg := group.Spec.Config; cfg != nil && cfg.PlayerLimit > 0 {
		limit = cfg.PlayerLimit
	}
	values.PlayerLimit = &limit
	onlineMode := true
	if cfg := group.Spec.Config; cfg != nil && cfg.OnlineMode != nil {
		onlineMode = *cfg.OnlineMode
	}
	values.OnlineMode = &onlineMode
	if cfg := group.Spec.Config; cfg != nil && cfg.Motd != "" {
		motd := cfg.Motd
		values.Motd = &motd
	}
	return values
}

// setProxyPodsBlocked records why a proxy pod this group asked for does not
// exist. It reports whether the condition transitioned to True on this call,
// so the caller can put an event on the flank rather than on every resync.
//
// This is the first writer of Degraded on a ProxyGroup. setStatus has always
// read it and routed it to phase Degraded; until now only ServerGroup ever
// set one.
//
// A condition already saying this exact thing is left byte-for-byte alone,
// and that is load-bearing rather than an optimisation. The refused-create
// caller passes the API server's own text, which names the pod it refused --
// and NewProxyName draws a fresh random suffix for every attempt, so the
// message differs on every single pass. Rewriting it made writeStatus bump
// resourceVersion every pass, which the For(&ProxyGroup{}) watch turned
// straight back into an enqueue, ahead of the rate-limited retry: a group
// whose pods the API server refuses -- exactly what a HostPort group in a
// baseline namespace is -- span at roughly 28 reconciles a second, forever.
// The milestone measured that as 3,940 refusals in a 139-second E2E run and
// filed the number as a normal cost; it was not.
//
// sameRefusal is what makes "this exact thing" mean the refusal rather than
// the string: two messages that differ only in the name of the object the
// API server refused are the same refusal, so the write is skipped, while a
// genuinely different refusal under the same reason -- a quota after a Pod
// Security fix, say -- still replaces the message. Nothing here edits what
// gets stored. The stored text stays the cluster's own, verbatim, because
// the remedy is in the API server's wording and nothing else knows it.
func setProxyPodsBlocked(group *spawneryv1alpha1.ProxyGroup, reason, message string) bool {
	// Read out, not held: FindStatusCondition returns a pointer into the
	// slice SetStatusCondition below writes through, so a verdict read after
	// that call is the value just written and never the value it replaced.
	existing := meta.FindStatusCondition(group.Status.Conditions,
		spawneryv1alpha1.ConditionDegraded)
	was := existing != nil && existing.Status == metav1.ConditionTrue
	if was && existing.Reason == reason && sameRefusal(existing.Message, message) {
		return false
	}
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionDegraded,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	return !was
}

// sameRefusal reports whether two cluster messages describe the same refusal,
// differing at most in the name of the object refused.
//
// The API server renders a refused create as
//
//	pods "gateway-kt84" is forbidden: violates PodSecurity "baseline:latest": hostPort ...
//
// where the first quoted run is the pod's name and everything after it is the
// remedy. The name is the only part that moves between two attempts at the
// same create, and it names a pod that never came into existence, so two
// messages that agree once it is elided are the same fact reported twice.
//
// This is a comparison and only a comparison: it elides nothing from the
// message that gets stored on the condition.
//
// The unschedulable half of Degraded carries no quoted run at all -- its
// message is "<pod> cannot be scheduled: <scheduler text>" -- so both sides
// pass through unchanged there and a different pod, or a different scheduler
// verdict on the same pod, is correctly seen as a different fact.
func sameRefusal(a, b string) bool {
	return elideFirstQuoted(a) == elideFirstQuoted(b)
}

func elideFirstQuoted(msg string) string {
	open := strings.Index(msg, `"`)
	if open < 0 {
		return msg
	}
	rest := strings.Index(msg[open+1:], `"`)
	if rest < 0 {
		return msg
	}
	return msg[:open+1] + msg[open+1+rest:]
}

// proxyPodsAdmittedMessage is the all-clear, and it is deliberately narrower
// than "every proxy pod this group asked for exists" -- the sentence it
// replaces, which claimed a count nothing on this path compares.
const proxyPodsAdmittedMessage = "the API server refused no proxy pod of this group, " +
	"and none is reported unschedulable"

// reportBlockedProxies is the second of the two ways a proxy pod fails to
// exist: the scheduler has nowhere to put it. With hostPort at most one pod
// of a group fits per node, so replicas is silently capped by the node count
// -- the likeliest HostPort mistake there is, and one that produces a pod
// that exists, never runs, and explains itself only on its own object.
//
// The pod's name is in the message because a group has several, and a
// condition that says only "a pod cannot be placed" cannot be acted on.
//
// It does not count nodes and predict. Doing that would mean reimplementing
// the scheduler's view of node selectors, taints and foreign hostPort
// holders in order to guess ahead of it, and being wrong the moment a node
// joins. Both halves of this condition report what the cluster said.
//
// The all-clear tail below fires an event too, on the same flank-only terms
// as setProxyPodsBlocked and matching ServerGroupReconciler's own
// BackingOff/Degraded pair: read the condition before writing it, write, and
// fire only when the write actually changed the verdict. Without this a
// group that recovers from a rejected or unschedulable proxy pod back to
// fully admitted did so silently — the Degraded condition would read False
// again, but nothing on the object's event timeline would say the recovery
// ever happened, unlike every other condition pair this reconciler reports.
func (r *ProxyGroupReconciler) reportBlockedProxies(
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) {
	for i := range pods {
		for _, c := range pods[i].Status.Conditions {
			if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse {
				continue
			}
			if setProxyPodsBlocked(group, spawneryv1alpha1.ReasonProxyPodUnschedulable,
				fmt.Sprintf("%s cannot be scheduled: %s", pods[i].Name, c.Message)) {
				r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, "ProxyPodBlocked",
					actionSyncStatus, "%s",
					eventNote("proxy pod %s cannot be scheduled: %s", pods[i].Name, c.Message))
			}
			return
		}
	}
	// What the pass actually established, and no more: reconcileReplicas
	// returned without the API server refusing anything, and no pod in the
	// list is reported unschedulable. It did not establish that the group has
	// as many pods as spec.replicas -- a create this pass suppressed by a
	// pending expectation is not refused, merely deferred -- so the sentence
	// must not say so. The condition's own semantics are unchanged: Degraded
	// answers "is a pod of this group blocked from existing", not "is this
	// group at size", which is what status.replicas is for.
	wasBlocked := meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionDegraded,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonProxyPodsAdmitted,
		Message: proxyPodsAdmittedMessage,
	})
	if wasBlocked {
		r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ProxyPodsAdmitted",
			actionSyncStatus, "%s", proxyPodsAdmittedMessage)
	}
}

// setStatus writes what is observably true of the group's pods.
//
// The two counts deliberately answer different questions about the same pods.
// readyReplicas is about the ready gate, so it counts only pods the kubelet
// calls Ready. connectedPlayers is about people, so it counts every pod in the
// group, ready or not — the CRD calls it "the sum of players across all
// proxies" and it is a printed column.
func (r *ProxyGroupReconciler) setStatus(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service) {
	var ready int32
	var players int32
	for i := range pods {
		// Outside the readiness guard below, and that placement is the whole
		// point: a draining proxy is NotReady on purpose and still has the
		// people this milestone exists to protect on it. Counting only ready
		// pods reported 0 during exactly the one operation where this field is
		// the only observable — nothing logs a readiness withdrawal anywhere —
		// so a real drain printed PLAYERS 0 with somebody in the game.
		//
		// The case the guard was written for is unaffected: a pod that is
		// starting up has told the registry nothing, and Lookup returns a zero
		// count for a pod it has not heard from, so it contributes 0 whether
		// the guard is here or not.
		//
		// The case that does change is a pod whose count is stale — an agent
		// that died with people on it, say. That one now contributes its last
		// known figure rather than nothing, which is a better answer than
		// zero and is the property this field already had for ready pods.
		// Both the drain event and status.connectedPlayers are last-reported
		// numbers, not measurements, and neither can be more than that.
		players += r.Agents.Lookup(string(pods[i].UID)).Players
		if !isPodReady(&pods[i]) {
			continue
		}
		ready++
	}

	group.Status.ReadyReplicas = ready
	group.Status.ConnectedPlayers = players
	group.Status.Address = proxyAddress(group, pods, svc)
	group.Status.ObservedGeneration = group.Generation

	switch {
	case meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded):
		group.Status.Phase = "Degraded"
	case ready >= group.Spec.Replicas && ready > 0:
		group.Status.Phase = string(phase.Ready)
	default:
		group.Status.Phase = string(phase.Pending)
	}
}

// readyHostIP is the node address of the first ready pod that has one. It is
// the readiness gate every strategy shares: nothing is published for a group
// whose pods cannot answer, which is what test/e2e/expose_test.go rests on --
// no image resolves there, so no pod is ready, so no address appears.
func readyHostIP(pods []corev1.Pod) string {
	for i := range pods {
		if isPodReady(&pods[i]) && pods[i].Status.HostIP != "" {
			return pods[i].Status.HostIP
		}
	}
	return ""
}

// readyHostIPBindingPort is the node address of the first ready pod whose
// container actually declares hostPort.
//
// This exists because a group's pods outlive the spec that created them. A
// group switched from NodePort to HostPort keeps its NodePort pods running and
// Ready until they are replaced -- and if the replacement is refused, forever.
// Asking only "is some pod ready" and then appending the spec's port published
// an address whose host was real, whose port was real, and which no process on
// that node was listening on. podspec.BuildProxyPod sets the container's
// HostPort only under the HostPort strategy (internal/podspec/proxy.go), so a
// pod from any other generation carries zero here and is skipped, which makes
// the distinction a fact about the pod rather than a rule to remember.
func readyHostIPBindingPort(pods []corev1.Pod, hostPort int32) string {
	// Zero matches every pod that declares no host port at all, which is every
	// pod of every other strategy -- so without this the helper would invert
	// its own purpose and hand back a node address for `host:0`. No object
	// reaching here can carry it: HostPortSpec.Port is a required non-pointer
	// field with +kubebuilder:validation:Minimum=1. The guard is for the
	// callers validation does not stand in front of, which is what this
	// package's own tests are.
	if hostPort == 0 {
		return ""
	}
	for i := range pods {
		if !isPodReady(&pods[i]) || pods[i].Status.HostIP == "" {
			continue
		}
		for _, c := range pods[i].Spec.Containers {
			for _, p := range c.Ports {
				if p.HostPort == hostPort {
					return pods[i].Status.HostIP
				}
			}
		}
	}
	return ""
}

// allocatedNodePort is the node port the API server assigned, read back off
// the Service rather than taken from the spec.
//
// The spec's port is only ever a request, and reconcileService's own
// NodePort case writes it straight into the Service it creates or updates —
// NodePortSpec.Port is required and `+kubebuilder:validation:Minimum=1`, so
// the API server never has to allocate one on the spec's behalf. What can
// happen is the Service itself being gone: a spec still naming a port proves
// nothing about whether anything is listening on it once the Service that
// carried it has been deleted, and the Service is the only place that says
// so. Matched by name, because that is what reconcileService sets and a
// group's Service carries exactly one port.
func allocatedNodePort(svc *corev1.Service) int32 {
	if svc == nil {
		return 0
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == podspec.MinecraftPortName {
			return p.NodePort
		}
	}
	return 0
}

// proxyAddress is where players connect.
//
// Every branch publishes an address only while a proxy is demonstrably
// serving, and that is the property the whole function is built around.
// Empty is the truthful answer otherwise: there is nowhere to connect yet,
// and printing an address for a proxy that is not serving would send players
// at a closed port.
//
// For NodePort and HostPort the address is a node's, and the operator has no
// right to read Node objects -- nor does it need one: hostIP on a ready
// proxy pod is the address of a node that demonstrably has a proxy on it,
// and the pod is already watched. Granting a cluster-wide node read for a
// status string would be the same trade the bootstrapper refused when it
// declined the update verb on ServiceAccounts to restore a cosmetic label.
//
// "A ready pod" is not the same claim as "a ready pod serving this
// strategy", and the two arms below no longer conflate them. HostPort asks
// readyHostIPBindingPort for a pod whose own container declares the port in
// question, not merely any ready pod -- see that function's comment for why
// a pod can be ready and still prove nothing about this strategy. NodePort
// asks allocatedNodePort for the port the Service actually carries, not
// group.Spec.Expose.NodePort.Port, because the spec is a request and the
// Service is what reconcileService and the API server made of it.
//
// For LoadBalancer the address comes from the Service instead, and the
// readiness gate has to be stated rather than inherited -- the Service knows
// nothing about whether anything is serving, so without the gate this would
// publish an address the moment a load balancer answered, including for a
// group whose every pod is in ImagePullBackOff.
//
// For ClusterIP the address is neither a node's nor the Service's: it is the
// string the operator wrote in the spec, echoed back unchanged, because the
// thing that owns it -- an ingress controller's TCP entry point, a gateway, a
// tunnel, a DNS record -- lives under an API this operator does not read.
//
// That arm is still behind a readiness gate, and that is the surprising
// part, because it reads neither of the things the gate tests. The gate
// needs a pod that is both Ready and carrying a non-empty hostIP, so a
// ClusterIP group publishes nothing while its pods are in ImagePullBackOff
// -- and also nothing for a pod that is Ready but whose hostIP has not been
// reported yet, a value this arm would never have used.
//
// This is deliberate, and is the same promise every other branch makes:
// status.address means "players can connect here now", not "this is what was
// configured". The configured value is already readable at
// spec.expose.clusterIP.address, so publishing it unconditionally would add
// no information while breaking the one invariant the field has. The
// consequence to accept is that this arm's answer is gated on facts about a
// pod that it never reads.
//
// net.JoinHostPort rather than a format string: a node with an IPv6 hostIP
// needs brackets, and the old formatting produced an address no client could
// use. For an IPv4 address the two are identical.
func proxyAddress(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service) string {
	port := func(p int32) string { return strconv.Itoa(int(p)) }

	switch group.Spec.Expose.Type {
	case spawneryv1alpha1.ExposeLoadBalancer:
		if svc == nil || readyHostIP(pods) == "" {
			return ""
		}
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				return net.JoinHostPort(ing.IP, port(podspec.MinecraftPort))
			}
			if ing.Hostname != "" {
				return net.JoinHostPort(ing.Hostname, port(podspec.MinecraftPort))
			}
		}
		return ""
	case spawneryv1alpha1.ExposeHostPort:
		if group.Spec.Expose.HostPort == nil {
			return ""
		}
		hostIP := readyHostIPBindingPort(pods, group.Spec.Expose.HostPort.Port)
		if hostIP == "" {
			return ""
		}
		return net.JoinHostPort(hostIP, port(group.Spec.Expose.HostPort.Port))
	case spawneryv1alpha1.ExposeClusterIP:
		// Echoed, not composed: no port is appended, because a Minecraft
		// client defaults to 25565 and "mc.example.test" is the whole of what
		// a player types. An operator who needs another port writes it in.
		//
		// The Service is required even though the address does not come from
		// it: this strategy publishes a name that something outside the
		// cluster routes to the Service, so without the Service the name goes
		// nowhere.
		if svc == nil || group.Spec.Expose.ClusterIP == nil || readyHostIP(pods) == "" {
			return ""
		}
		return group.Spec.Expose.ClusterIP.Address
	case spawneryv1alpha1.ExposeNodePort:
		nodePort := allocatedNodePort(svc)
		if nodePort == 0 {
			return ""
		}
		hostIP := readyHostIP(pods)
		if hostIP == "" {
			return ""
		}
		return net.JoinHostPort(hostIP, port(nodePort))
	default:
		// See reconcileService's default: for why this is written out rather
		// than folded into the NodePort arm.
		return ""
	}
}

// isPodReady reports what the kubelet says about the pod's readiness probe.
// For a proxy that is the whole ready gate: design 6.6 has the agent serve the
// probe itself and only turn it green after it has processed its FullSync, so
// this condition already carries the answer the registry would otherwise be
// asked for.
func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// exposeImplemented reports whether this operator has a branch for the
// strategy. See the call site in Reconcile for why it exists at all, and
// TestExposeImplementedCoversTheEnumAndNothingElse for why it is a function
// rather than an inline default arm.
//
// Adding a strategy means editing four things that must agree and are not
// checked against each other by anything: the CRD's enum, this list, the
// switch in reconcileService, and the switch in proxyAddress. Nothing
// automated enforces the agreement -- the enum is closed, so no test can
// construct an object that exercises a disagreement through the API, which is
// exactly why the disagreement can be committed green. Stated here because
// this is the list a fifth strategy's author edits first, and the two
// default: arms are what catch the half-done edit: reconcileService returns
// an error naming the type, proxyAddress returns "".
func exposeImplemented(t spawneryv1alpha1.ExposeType) bool {
	switch t {
	case spawneryv1alpha1.ExposeNodePort,
		spawneryv1alpha1.ExposeLoadBalancer,
		spawneryv1alpha1.ExposeHostPort,
		spawneryv1alpha1.ExposeClusterIP:
		return true
	default:
		return false
	}
}

// setProxyGroupAccepted records whether the operator manages this group.
func setProxyGroupAccepted(group *spawneryv1alpha1.ProxyGroup, ok bool, reason, message string) {
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  conditionStatus(ok),
		Reason:  reason,
		Message: message,
	})
}

func (r *ProxyGroupReconciler) writeStatus(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	return r.Status().Update(ctx, group)
}

// groupsOnNode maps a Node event onto the ProxyGroups with pods on that node.
//
// Mirrors ServerGroupReconciler.groupsOnNode: the five-second resync would
// find a cordoned node on its own, and the watch is what makes the answer
// immediate. It lists this operator's pods and filters by node rather than
// asking for a spec.nodeName field index, for the same reason that function
// gives — an index shared by two controllers would have to be registered
// once, registering it twice fails at manager start, and a label-scoped list
// over a warm cache is cheaper than that coordination is worth. The role
// label pins RoleProxy rather than only ManagedBy, so a server pod that
// happens to share a node is never in the candidate set to begin with —
// neither network nor group is known yet at this point, which is what
// r.pods's podspec.ProxyLabels selector has and this one does not, so the
// two fields it can name are the ones ProxyLabels always sets regardless.
func (r *ProxyGroupReconciler) groupsOnNode(ctx context.Context, obj client.Object) []reconcile.Request {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleProxy,
	}); err != nil {
		// Enqueue nothing rather than guess. A map function has no way to
		// return an error and no queue of its own to retry from, so dropping
		// the event is the only option here; the five-second resync is the
		// fallback, and it reaches the same conclusion from reconcileReplicas.
		// That costs this cordon its immediacy, which is the whole point of
		// the watch, so it is logged rather than swallowed — the same rule
		// nodeDeparting follows when it cannot read a node, and the same
		// choice ServerGroupReconciler.groupsOnNode makes for the same reason.
		log.FromContext(ctx).V(1).Info("listing proxy pods for a node event failed, "+
			"leaving this cordon to the resync", "node", obj.GetName(), "error", err)
		return nil
	}
	seen := map[types.NamespacedName]bool{}
	var out []reconcile.Request
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != obj.GetName() {
			continue
		}
		group := pod.Labels[podspec.LabelGroup]
		if group == "" {
			continue
		}
		key := types.NamespacedName{Name: group, Namespace: pod.Namespace}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, reconcile.Request{NamespacedName: key})
	}
	return out
}

// SetupWithManager registers the controller.
func (r *ProxyGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A construction site that forgets either of these would otherwise panic
	// inside a reconcile, in a goroutine, minutes after start — the same
	// failure mode SetupAll refuses a nil Bootstrapper for, and the same guard
	// ServerGroupReconciler.SetupWithManager carries for Expectations. Cheap
	// insurance twice over here: 4c-1 shipped exactly this defect once, and
	// Divergence.forget now sits on five of Reconcile's paths, every one of
	// which returns before reconcileReplicas — so a nil field would be
	// dereferenced there first, and the earliest of the five is reachable on
	// the very first reconcile of a group that is already gone.
	if r.Expectations == nil {
		r.Expectations = newExpectations(r.Clock)
	}
	if r.Divergence == nil {
		r.Divergence = newReadinessDivergence(r.Clock)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.ProxyGroup{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.groupsOnNode)).
		Complete(r)
}
