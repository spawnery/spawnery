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
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
// pod to stop taking connections, as an RFC 3339 timestamp. On the pod because
// a proxy has no CR of its own and the date must survive an operator restart.
const ProxyDrainingSinceAnnotation = podspec.AnnotationProxyDrainingSince

// readinessDivergenceGrace is how long a pod's actual readiness may disagree
// with the asserted one before the group says so. It clears the probe's 10 to
// 15 seconds (period 5s times threshold 3) and Fleet.Resync's 30 seconds.
const readinessDivergenceGrace = 60 * time.Second

// ProxyReadinessSetter tells one proxy pod whether it should be taking
// connections. *proxyreg.Fleet satisfies it.
type ProxyReadinessSetter interface {
	SetReady(ctx context.Context, podUID string, ready bool) error
}

// NewProxyName builds a unique proxy pod name below the group prefix.
func NewProxyName(group string) string { return NewServerName(group) }

// ProxyGroupReconciler keeps a proxy group at its replica count, keeps its
// Service in step, and publishes where players connect.
//
// Unlike ServerGroupReconciler it manages pods directly: proxies are fungible,
// with no per-proxy object and no state machine. Draining one means telling
// its agent to stop taking connections, dating that, and removing the pod
// once it is empty or its deadline has passed.
//
// Group events: NodeDraining (a proxy marked off a departing node),
// ProxyDrainTimeout (the one thing here that disconnects a player),
// ReadinessDiverged/ReadinessAgrees and ProxyPodBlocked/ProxyPodsAdmitted on
// the flanks of their conditions. Pod events, which let the in-game feed name
// the proxy: ProxyStarted, ProxyRetiring, ProxyStopped.
type ProxyGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	Agents *agent.Registry
	// Bootstrap puts the CA bundle and the ServiceAccounts into the namespace
	// before the first pod is created there.
	Bootstrap *Bootstrapper
	// AgentEndpoint is the address the in-game agent dials.
	AgentEndpoint string
	// OperatorNamespace is where the operator runs; the group's egress
	// policy names it so a proxy may dial the agent port.
	OperatorNamespace string
	Proxies           ProxyReadinessSetter
	Clock             func() time.Time
	Recorder          events.EventRecorder
	// Expectations reserves the pod creates and deletes this reconciler has
	// issued and the cache has not shown yet. One instance is shared across
	// groups.
	Expectations *expectations
	// Divergence tracks how long each pod's actual readiness has disagreed with
	// the asserted one. Shared across groups.
	Divergence *readinessDivergence
	// DrainTaintKeys is Options.DrainTaintKeys. Nil means only cordoned nodes
	// count.
	DrainTaintKeys []string

	// AllowPluginVolumes is Options.AllowPluginVolumes.
	AllowPluginVolumes bool

	// AllowFileVolumes is Options.AllowFileVolumes.
	AllowFileVolumes bool

	// AllowMountVolumes is Options.AllowMountVolumes.
	AllowMountVolumes bool

	// ClaimReader reads a group's spec.extraPlugins claim, and it must be
	// uncached; see ServerGroupReconciler.ClaimReader.
	ClaimReader client.Reader
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
			// No ProxyGroup finalizer exists, so most deletions are only seen as
			// NotFound; nothing would ever observe these keys again.
			r.Expectations.forget(req.Namespace + "/" + req.Name)
			r.Divergence.forget(req.Namespace + "/" + req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// The pods and the Service are owned and go with the group.
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

	// After the network checks: a missing Network is the bigger problem.
	reason, message, ok := checkGroupVolumes(
		ctx, r.ClaimReader, group.Namespace,
		group.Spec.ExtraPlugins, group.Spec.ExtraFiles, group.Spec.Mounts,
		r.AllowPluginVolumes, r.AllowFileVolumes, r.AllowMountVolumes)
	if reason == reasonClaimUnreadable {
		log.FromContext(ctx).Info("a claim could not be read; keeping the group's last decision",
			"group", group.Name, "problem", message)
		reason, message, ok = keepLastVolumeDecision(group.Status.Conditions, reason, message, ok)
	}
	if !ok {
		// Announced on the transition only; this runs every pass.
		if !hasConditionReason(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted, reason) {
			r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, reason, actionSyncStatus,
				"%s", message)
		}
		setProxyGroupAccepted(group, false, reason, message)
		return r.refuse(ctx, group)
	}

	// Unreachable while the CRD's enum is closed; it catches a new enum value
	// without a branch, as a refusal a user can read.
	if !exposeImplemented(group.Spec.Expose.Type) {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonExposeNotImplemented,
			fmt.Sprintf("expose.type %s is not implemented by this operator",
				group.Spec.Expose.Type))
		// refuse, not a bare return: the player-safety pass must keep running.
		return r.refuse(ctx, group)
	}
	if message, ok := podspec.SchedulingRefusal(network,
		podspec.EffectiveScheduling(network, group.Spec.Scheduling), group.Namespace); !ok {
		if !hasConditionReason(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted, spawneryv1alpha1.ReasonSchedulingNotAllowed) {
			r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, spawneryv1alpha1.ReasonSchedulingNotAllowed, actionSyncStatus,
				"%s", message)
		}
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonSchedulingNotAllowed, message)
		return r.refuse(ctx, group)
	}
	if group.Spec.Expose.Type == spawneryv1alpha1.ExposeHostPort && group.Spec.Expose.HostPort != nil {
		if message, ok := podspec.HostPortRefusal(network, group.Spec.Expose.HostPort.Port); !ok {
			if !hasConditionReason(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted, spawneryv1alpha1.ReasonHostPortNotAllowed) {
				r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, spawneryv1alpha1.ReasonHostPortNotAllowed, actionSyncStatus,
					"%s", message)
			}
			setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonHostPortNotAllowed, message)
			return r.refuse(ctx, group)
		}
	}
	setProxyGroupAccepted(group, true, spawneryv1alpha1.ReasonAccepted, "")
	// Persisted before the side effects, so a failure below (a NodePort
	// collision, say) does not leave an object with no conditions at all.
	if err := r.writeStatus(ctx, group); err != nil {
		return ctrl.Result{}, err
	}

	obs, res, err := r.reconcileObserved(ctx, network, group)

	// A foreign ConfigMap at the rendered name stops the pass before any pod is
	// read, for good; without this it would requeue in silence. The pods and the
	// Service are left alone, as on the missing-Network path.
	if errors.Is(err, errForeignConfigMap) {
		name := podspec.GroupConfigMapName(group.Name, podspec.RoleProxy)
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  spawneryv1alpha1.ReasonConfigMapNotOurs,
			Message: foreignConfigMapMessage(group.Namespace, name),
		})
		group.Status.Phase = "Degraded"
		// refuse: the budget and a departing node do not depend on the
		// ConfigMap. Requeued, as the collision has no owner reference here.
		return r.refuse(ctx, group)
	}

	// The status is written wherever the pods and the Service were read, so a
	// pass failing partway does not keep advertising what an earlier pass saw;
	// and nowhere else, so a missing Network does not blank a working address.
	if obs.observed {
		r.setStatus(group, obs.pods, obs.svc)
		if werr := r.writeStatus(ctx, group); werr != nil {
			if err == nil {
				return res, werr
			}
			log.FromContext(ctx).Error(werr, "recording the group's status")
		}
	}
	return res, err
}

// proxyObservation is what one pass of reconcileObserved saw. observed is a
// flag because HostPort creates no Service, so a nil svc is normal there.
type proxyObservation struct {
	observed bool
	pods     []corev1.Pod
	svc      *corev1.Service
}

// reconcileObserved does everything from the namespace bootstrap onward and
// returns what it saw, so the caller can record the status either way.
func (r *ProxyGroupReconciler) reconcileObserved(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
) (proxyObservation, ctrl.Result, error) {
	var obs proxyObservation

	if err := r.Bootstrap.Ensure(ctx, group.Namespace); err != nil {
		return obs, ctrl.Result{}, err
	}
	// Every proxy pod mounts this ConfigMap by name; the early return keeps
	// reconcileReplicas from creating one before it exists.
	if err := r.reconcileConfigMap(ctx, group); err != nil {
		return obs, ctrl.Result{}, err
	}
	svc, err := r.reconcileService(ctx, group)
	if err != nil {
		return obs, ctrl.Result{}, err
	}
	if err := r.reconcileNetworkPolicy(ctx, group); err != nil {
		return obs, ctrl.Result{}, err
	}
	// A snapshot from before this pass's own creates: a pod created below is
	// first seen by the per-pod logic on the next pass.
	pods, err := r.pods(ctx, group)
	if err != nil {
		return obs, ctrl.Result{}, err
	}
	if err := r.announceReady(ctx, pods); err != nil {
		return obs, ctrl.Result{}, err
	}
	obs = proxyObservation{observed: true, pods: pods, svc: svc}

	if err := r.reconcileReplicas(ctx, network, group, pods); err != nil {
		// A refused create belongs on the group: Pod Security (every HostPort
		// group in a baseline or restricted namespace), RBAC, quota, a webhook.
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			if setProxyPodsBlocked(group, spawneryv1alpha1.ReasonProxyPodRejected, err.Error()) {
				r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, "ProxyPodBlocked",
					actionCreateProxyPod, "%s",
					eventNote("the API server refused a proxy pod: %s", err.Error()))
			}
			// setStatus derives Degraded from this condition; the caller writes.
		}
		return obs, ctrl.Result{}, err
	}

	// Re-read after the changes, so the status describes what is there now.
	if pods, err = r.pods(ctx, group); err != nil {
		return obs, ctrl.Result{}, err
	}
	obs.pods = pods

	// Before setStatus, which reads Degraded.
	r.reportBlockedProxies(group, pods)
	// On the re-read pods, so a replacement created this pass counts.
	reportGroupRotation(&group.Status.Conditions, network.Status.ForwardingSecretHash, pods)
	if err := r.protectOccupiedProxies(ctx, group, pods); err != nil {
		return obs, ctrl.Result{}, err
	}
	return obs, ctrl.Result{RequeueAfter: ResyncInterval}, nil
}

// refuse is the shared tail of the paths that give up before
// reconcileReplicas; the caller has already set the Accepted reason.
//
// Divergence is not forgotten: protectPlayersOnly still withdraws readiness
// from pods on departing nodes, and a proxy can ignore that here as anywhere.
// The status is written even when protectPlayersOnly fails, or a permanent
// failure there would hide the refusal. The requeue is networkRetryInterval;
// controller-runtime backs off instead on an error.
func (r *ProxyGroupReconciler) refuse(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) (ctrl.Result, error) {
	// A Waiting group gives up its changeover place, which AdmitChangeovers
	// would otherwise keep handing to it. A Begun one is never paused halfway.
	if group.Status.Changeover == spawneryv1alpha1.ChangeoverWaiting {
		group.Status.Changeover = spawneryv1alpha1.ChangeoverNone
	}
	group.Status.ObservedGeneration = group.Generation
	protectErr := r.protectPlayersOnly(ctx, group)
	if err := r.writeStatus(ctx, group); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: networkRetryInterval}, protectErr
}

// protectOccupiedProxies keeps the eviction API off a proxy with players: the
// occupied label first, then the PodDisruptionBudget whose selector matches it,
// sized from syncOccupiedLabels's own tally rather than a second registry read
// (see proxyOccupied).
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
// that give up before reconcileReplicas: the occupied label, the budget and
// the NodeDraining condition, which do not depend on the Network (the rule
// ServerGroupReconciler.Reconcile states). Without it a broken Network would
// freeze the budget, and a player joining later could be evicted.
//
// These paths requeue at networkRetryInterval, so a newly occupied proxy can
// wait that long to be counted.
func (r *ProxyGroupReconciler) protectPlayersOnly(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	pods, err := r.pods(ctx, group)
	if err != nil {
		return err
	}
	nodeGoing := make([]bool, len(pods))
	// Only departing-node pods: a surplus or stale hash needs a replacement the
	// refused group cannot build, and can wait.
	leaving := make(map[string]bool, len(pods))
	for i := range pods {
		nodeGoing[i] = nodeDeparting(ctx, r.Client, pods[i].Spec.NodeName, r.DrainTaintKeys)
		if nodeGoing[i] {
			leaving[pods[i].Name] = true
		}
	}
	// The sentence follows the Accepted reason rather than always blaming the
	// Network.
	blocked := blockedReplacement{Reason: "its Network is missing or not accepted"}
	if c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted); c != nil &&
		c.Reason != spawneryv1alpha1.ReasonNetworkNotFound && c.Reason != spawneryv1alpha1.ReasonNetworkNotAccepted {
		blocked.Reason = "it is not accepted (" + c.Reason + ")"
	}
	r.reportNodeDraining(group, pods, nodeGoing, blocked)
	// Protect before the drain, so this pass's budget describes the fleet the
	// drain then acts on.
	if err := r.protectOccupiedProxies(ctx, group, pods); err != nil {
		return err
	}
	return r.drainDeparting(ctx, group, pods, leaving, nodeGoing, "")
}

// pods lists the group's live proxy pods, oldest first. The filter is
// podspec.ProxyLabels, the same map reconcileService uses as the Service
// selector.
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
	r.Expectations.observePods(group.Namespace+"/"+group.Name, live)
	return live, nil
}

// proxyOccupied is the occupancy rule for a proxy pod. Ask it (or
// proxyOccupiedForBudget) once per pod per pass: Lookup re-derives PlayersStale
// from the clock, so two calls can disagree.
//
// A proxy has no wasRegistered qualifier: players reach it directly through
// the Service. A down stream counts too, because Velocity keeps serving its
// sessions after its agent's stream breaks. Read unqualified only by the
// deletion wait, which drain.timeoutSeconds bounds.
func proxyOccupied(snap agent.Snapshot) bool {
	return snap.Players != 0 || snap.PlayersStale || !snap.Connected
}

// proxyPlayerNote says what is known about who is on a proxy the deadline is
// about to disconnect. A phrase, not a number: Lookup returns the last report,
// so a dead agent reads as its last count and a never-connected one as zero.
func proxyPlayerNote(snap agent.Snapshot) string {
	switch {
	case !snap.Known:
		return "no agent ever reported from it, so who was on it is unknown"
	case snap.PlayersStale:
		return fmt.Sprintf("its last report, already stale, said %d player(s)", snap.Players)
	default:
		return fmt.Sprintf("%d player(s) still connected", snap.Players)
	}
}

// proxyOccupiedForBudget is proxyOccupied for the label and the budget, which
// no deadline bounds. A pod the registry never heard of reads as occupied, and
// a replacement stuck in CrashLoopBackOff would block every eviction forever.
// Such a pod cannot hold players: its readiness probe is served by its own
// agent, so it is no Service endpoint. Registry.Disconnect keeps Known true,
// so a pod whose agent died still counts. Right after an operator restart every
// pod is unknown, so an unknown pod counts as occupied within
// budgetReconnectGrace.

// budgetReconnectGrace is the fleet's reconnect time after an operator
// restart, the same question the Server side's ready gate asks.
const budgetReconnectGrace = phase.ReconnectGrace

func proxyOccupiedForBudget(snap agent.Snapshot) bool {
	if !snap.Known && snap.StreamDownFor >= budgetReconnectGrace {
		return false
	}
	return proxyOccupied(snap)
}

// syncOccupiedLabels keeps podspec.LabelOccupied in step with
// proxyOccupiedForBudget and returns how many pods it found occupied, which
// reconcileProxyPDB sizes minAvailable from so label and budget agree pod for
// pod.
func (r *ProxyGroupReconciler) syncOccupiedLabels(ctx context.Context, pods []corev1.Pod) (int32, error) {
	var occupiedCount int32
	for i := range pods {
		pod := &pods[i]
		occupied := proxyOccupiedForBudget(r.Agents.Lookup(string(pod.UID)))
		if occupied {
			occupiedCount++
		}
		_, labelled := pod.Labels[podspec.LabelOccupied]
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
		// NotFound: the read may predate this pass's own delete of a pod that just
		// emptied.
		if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return occupiedCount, nil
}

// reconcileProxyPDB keeps the group's PodDisruptionBudget in step with
// syncOccupiedLabels's tally, as an absolute minAvailable like the
// ServerGroup's. Without it kubectl drain evicts a proxy the second the node
// is cordoned, while its replacement is still pulling its image.
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
		// ProxyLabels returns a fresh map, so this does not reach the Service.
		selector := podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
		selector[podspec.LabelOccupied] = "true"
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		return controllerutil.SetControllerReference(group, pdb, r.Scheme)
	})
	return err
}

// reconcileReplicas creates or removes pods until the count matches the spec,
// and replaces pods that are stale: whose rendered shape no longer matches the
// group, that were asked to retire, or that sit on a departing node. Which pods
// go is DecideRollout's answer.
func (r *ProxyGroupReconciler) reconcileReplicas(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) error {
	configValues, err := yaml.Marshal(proxyConfigValues(group))
	if err != nil {
		return err
	}
	wantHash, err := podspec.DesiredProxyHash(network, group, r.AgentEndpoint, configValues)
	if err != nil {
		return err
	}

	// Asked once per pod and reused, since two asks can disagree.
	nodeGoing := make([]bool, len(pods))
	views := make([]ProxyView, 0, len(pods))
	for i := range pods {
		snap := r.Agents.Lookup(string(pods[i].UID))
		_, dated := drainingSince(&pods[i])
		nodeGoing[i] = nodeDeparting(ctx, r.Client, pods[i].Spec.NodeName, r.DrainTaintKeys)
		requested := pods[i].Annotations[podspec.AnnotationRetireRequested] != ""
		views = append(views, ProxyView{
			Name:            pods[i].Name,
			Stale:           pods[i].Labels[podspec.LabelPodHash] != wantHash || nodeGoing[i] || requested,
			RetireRequested: requested,
			Ready:           isPodReady(&pods[i]),
			Draining:        dated,
			Players:         snap.Players,
			PlayersStale:    snap.PlayersStale,
			CreatedAt:       pods[i].CreationTimestamp.Time,
		})
	}
	// Reports where pods are, not what was decided; the per-proxy event fires
	// when one is marked. Nothing blocks replacement once we get this far.
	r.reportNodeDraining(group, pods, nodeGoing, blockedReplacement{})
	key := group.Namespace + "/" + group.Name
	pendingCreates, _, _ := r.Expectations.pending(key)
	own, surgeAllowed, wait, err := proxyChangeover(ctx, r, network, group, wantHash, int32(len(pendingCreates)))
	if err != nil {
		return err
	}
	group.Status.Changeover = own
	reportChangingOver(group, pods, wantHash, wait)

	decision := DecideRollout(views, group.Spec.Replicas, surgeAllowed)

	// The views come from the cache, which may not show this reconciler's own
	// creates yet; subtracting exactly the reserved creates keeps a legitimate
	// further create possible.
	create := decision.Create - int32(len(pendingCreates))
	if create < 0 {
		create = 0
	}
	// Reservations made before a failure in this loop stand on purpose: those
	// pods exist, and the retry must not duplicate them.
	for i := int32(0); i < create; i++ {
		pod, err := podspec.BuildProxyPod(network, group, NewProxyName(group.Name), r.AgentEndpoint, configValues)
		if err != nil {
			return err
		}
		if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		r.Expectations.expectCreated(key, pod.Name, 0)
	}
	if own == spawneryv1alpha1.ChangeoverWaiting && decision.Create > 0 {
		group.Status.Changeover = spawneryv1alpha1.ChangeoverBegun
	}

	// Which pods are going, derived rather than read off the annotation, which
	// a pod marked this pass does not carry yet.
	leaving := make(map[string]bool, len(decision.Drain))
	for _, name := range decision.Drain {
		leaving[name] = true
	}
	// DecideRollout names only pods to mark now, so existing marks are kept here,
	// or every drain would be cancelled and its deadline restarted. A stale mark
	// is kept per pod. Surplus belongs to no pod, so only as many surplus marks
	// are kept as
	//
	//	len(views) - staleMarks - keptSurplusMarks >= replicas
	//
	// allows. Stale marks rather than stale pods: after a reverted spec change the
	// marked pods are surplus and the unmarked replacements stale, and subtracting
	// stale pods would release the marks at once. Holding them costs up to
	// replicas extra proxies, bounded by the deadline;
	// TestARevertedSpecChangeKeepsTheMarkItAlreadyMade pins it. Counting rather
	// than persisting marks keeps a reversed scale-down reversible on the pod.
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
	// pick keeps the marks a fresh DecideRollout would make, since it sorts with
	// the same comparator. A released pod loses its annotation on the same pass
	// and leaves the candidate set, so this cannot oscillate.
	//
	// Open: a create the cache has not shown makes len(views), and so want, one
	// low, releasing one mark too many and restarting that drain's deadline. Four
	// candidate states were traced without reaching it; that is no proof.
	if want := int32(len(views)) - staleMarks - group.Spec.Replicas; want > 0 {
		for _, name := range pick(surplusMarks, want) {
			leaving[name] = true
		}
	}

	return r.drainDeparting(ctx, group, pods, leaving, nodeGoing, wantHash)
}

// drainDeparting is a proxy pod's removal once something decided it should go:
// withdraw its readiness, stamp the draining mark that starts its deadline,
// and delete it once it is empty or the deadline has passed.
//
// reconcileReplicas passes what DecideRollout nominated; protectPlayersOnly
// passes only departing-node pods. A refused group cannot replace what it
// removes, but a departing node cannot wait: without this the budget would
// refuse the eviction while nothing acted on it, and kubectl drain would hang
// until the Network is fixed.
//
// leaving is recomputed every pass, so a node whose taint is removed stops
// being a reason and the mark is cleared.
func (r *ProxyGroupReconciler) drainDeparting(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
	leaving map[string]bool,
	nodeGoing []bool,
	wantHash string,
) error {
	key := group.Namespace + "/" + group.Name
	// The desired readiness is derived, not remembered, so a restart or a
	// cancelled scale-down corrects itself.
	//
	// Only the withdrawal direction counts as divergence: an agent told to stop
	// that is still Ready. Asserted ready but not Ready yet is a pod starting up,
	// and a cold image pull can outrun the grace.
	diverging := make(map[types.UID]bool, len(pods))
	names := make(map[types.UID]string, len(pods))
	for i := range pods {
		going := leaving[pods[i].Name]
		// Read before markDraining below changes it.
		_, wasMarked := drainingSince(&pods[i])
		if err := r.Proxies.SetReady(ctx, string(pods[i].UID), !going); err != nil {
			return err
		}
		if err := r.markDraining(ctx, &pods[i], going); err != nil {
			return err
		}
		// Once per proxy, and only when the node is a reason it was marked.
		if going && !wasMarked && nodeGoing[i] {
			r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, spawneryv1alpha1.ReasonNodeDraining, actionDrainProxy,
				"draining proxy %s off a node that is going away", pods[i].Name)
		}
		if going && !wasMarked && !nodeGoing[i] {
			why := "scaled down"
			switch {
			case pods[i].Annotations[podspec.AnnotationRetireRequested] != "":
				why = "retire requested"
			case pods[i].Labels[podspec.LabelPodHash] != wantHash:
				why = "rolling update"
			}
			r.Recorder.Eventf(&pods[i], nil, corev1.EventTypeNormal, "ProxyRetiring", actionDrainProxy,
				"proxy %s takes no new connections and stops once empty: %s", pods[i].Name, why)
		}
		diverging[pods[i].UID] = going && isPodReady(&pods[i])
		names[pods[i].UID] = pods[i].Name
	}
	r.reportReadinessDivergence(group, key, diverging, names)

	// Removal waits for the pod to be empty: NotReady stops new connections,
	// but Kubernetes does not close the sessions already on it. The deadline is
	// the only path here that disconnects anyone.
	//
	// Empty means fresh, zero, and from a stream that is still up. Velocity keeps
	// serving after its agent's stream breaks, and Registry.Disconnect keeps the
	// last count, so a zero from a dead stream is believed for 2x the report
	// interval while players can still join.
	//
	// Unlike isOccupied there is no sessionsGone term: a crashed surplus proxy
	// waits out its deadline and is announced as losing players the crash already
	// disconnected. Conservative and bounded.
	for i := range pods {
		if !leaving[pods[i].Name] {
			continue
		}
		pod := &pods[i]
		snap := r.Agents.Lookup(string(pod.UID))
		// The loop above has already stamped every leaving pod, so the deadline
		// starts this pass at the latest.
		since, dated := drainingSince(pod)
		waited := r.Clock().Sub(since)
		// Measured from when the count became unknown, not from the drain's
		// start: after an operator restart every count is unknown until its
		// agent reconnects, and a long soft drain must survive that.
		var unknownFor time.Duration
		switch {
		case !snap.Connected:
			unknownFor = snap.StreamDownFor
		case snap.PlayersStale:
			unknownFor = r.Clock().Sub(snap.PlayersReportedAt)
		}
		expired := dated && ((nodeGoing[i] && waited >= group.DrainTimeout()) ||
			(unknownFor > 0 && unknownFor >= group.DrainTimeout() && waited >= group.DrainTimeout()) ||
			(group.MaxStale() > 0 && waited >= group.MaxStale()))

		// Announced only once the delete lands: a pass that failed to delete
		// disconnected nobody.
		var announce func()
		switch {
		case !proxyOccupied(snap):
			announce = func() {
				r.Recorder.Eventf(pod, nil, corev1.EventTypeNormal, "ProxyStopped", actionDrainProxy,
					"proxy %s stopped: no players left", pod.Name)
			}
		case expired:
			// The one path that disconnects anybody, so it says so loudly.
			announce = func() {
				r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, "ProxyDrainTimeout", actionDrainProxy,
					"deleting proxy %s after %s: %s",
					pod.Name, group.DrainTimeout(), proxyPlayerNote(snap))
			}
		default:
			continue
		}
		if err := r.Delete(ctx, pod); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			// Already gone: still reserved, but nobody was disconnected by this pass.
			announce = nil
		}
		if announce != nil {
			announce()
		}
		r.Expectations.expectDeleted(key, pod.Name)
	}
	return nil
}

// reportReadinessDivergence reports a pod told to stop taking connections that
// the kubelet still calls Ready past readinessDivergenceGrace: an agent that
// got the withdrawal and did not act. A lost SetReady heals on the next resync;
// this only reports. The event goes on the flank.
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
// among this pass's live pods, are departing. nodeGoing is the caller's own
// per-pod verdict, indexed like pods. No event: a second departing node while
// the condition is True would not flip it; the per-proxy event covers that.
func (r *ProxyGroupReconciler) reportNodeDraining(
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
	nodeGoing []bool,
	blocked blockedReplacement,
) {
	names := make([]string, 0, len(pods))
	for i, going := range nodeGoing {
		if going {
			names = append(names, pods[i].Spec.NodeName)
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions, drainingConditionBlocked(names, blocked))
}

// reportChangingOver sets ConditionChangingOver from how many of the group's
// live pods carry a hash this operator no longer renders; a pod with no hash
// label counts. No event, as in reportNodeDraining: the count can grow without
// a transition.
func reportChangingOver(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, wantHash string, wait ChangeoverWait) {
	stale := 0
	for i := range pods {
		if pods[i].Labels[podspec.LabelPodHash] != wantHash {
			stale++
		}
	}
	cond := metav1.Condition{
		Type:    spawneryv1alpha1.ConditionChangingOver,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonPodShapeCurrent,
		Message: "every proxy pod carries the shape this operator renders",
	}
	if stale > 0 {
		cond.Status = metav1.ConditionTrue
		cond.Reason = spawneryv1alpha1.ReasonPodShapeChanged
		cond.Message = fmt.Sprintf(
			"%d of %d proxy pods carry a shape this operator no longer renders and are "+
				"being replaced; if every group in the cluster says this at "+
				"once, an operator upgrade changed the pod render rather than anyone "+
				"editing a spec", stale, len(pods))
		if wait.Message != "" {
			cond.Message = wait.Message
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions, cond)
}

// drainingSince reads the annotation markDraining writes. ok is false when it
// is missing or does not parse. Not an error: anyone who can annotate a pod
// can write a bad value, and an error would abort every pass forever.
// markDraining re-stamps such a pod.
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
// Written once and never moved while readable, or the deadline would never
// arrive; so the write keys on drainingSince, not on presence. An unparsable
// stamp is rewritten, which restarts that pod's clock: it can wait up to one
// drain timeout longer. Removal keys on presence, so a bad stamp is cleared
// too. NotFound is tolerated for a pod deleted since the list.
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

// announceReady dates each proxy's first pass of the ready gate on the pod and
// records ProxyStarted for it, once: the date is what says it was announced.
func (r *ProxyGroupReconciler) announceReady(ctx context.Context, pods []corev1.Pod) error {
	for i := range pods {
		pod := &pods[i]
		if !isPodReady(pod) || pod.Annotations[podspec.AnnotationProxyReadySince] != "" {
			continue
		}
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[podspec.AnnotationProxyReadySince] = r.Clock().UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, pod, patch); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		r.Recorder.Eventf(pod, nil, corev1.EventTypeNormal, "ProxyStarted", actionSyncStatus,
			"proxy %s is taking connections", pod.Name)
	}
	return nil
}

// reconcileService keeps the group's Service in step with its expose strategy
// and returns it, for status.loadBalancer. HostPort gets no Service and
// returns nil: nothing inside the cluster dials a proxy.
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

		// Unconditional: leaving LoadBalancer releases the keys with nil.
		var lbAnnotations map[string]string
		if group.Spec.Expose.Type == spawneryv1alpha1.ExposeLoadBalancer &&
			group.Spec.Expose.LoadBalancer != nil {
			lbAnnotations = group.Spec.Expose.LoadBalancer.Annotations
		}
		applyExposeAnnotations(svc, lbAnnotations)

		// The selector pins the role, or same-named server pods would be selected.
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
			// No node port named: the API server allocates one, and naming it
			// would let two groups collide over a number no player dials.
		case spawneryv1alpha1.ExposeClusterIP:
			// No external traffic policy: the API server rejects it on ClusterIP.
			svc.Spec.Type = corev1.ServiceTypeClusterIP
		case spawneryv1alpha1.ExposeNodePort:
			// Local: Cluster SNATs, and Velocity needs players' real IPs for bans
			// and rate limits. A node without a proxy pod then answers nothing,
			// but proxyAddress only publishes nodes that run a ready one.
			svc.Spec.Type = corev1.ServiceTypeNodePort
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			port.NodePort = group.Spec.Expose.NodePort.Port
		default:
			// Unreachable while exposeImplemented and this switch agree.
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

// loadBalancerTrafficPolicy is the CRD's Local default, restated for objects
// that never passed the API server's defaulting.
func loadBalancerTrafficPolicy(group *spawneryv1alpha1.ProxyGroup) corev1.ServiceExternalTrafficPolicy {
	if lb := group.Spec.Expose.LoadBalancer; lb != nil && lb.ExternalTrafficPolicy != "" {
		return lb.ExternalTrafficPolicy
	}
	return corev1.ServiceExternalTrafficPolicyLocal
}

// deleteServiceIfOurs removes the Service a group had before it switched to
// HostPort, and only that one: controlled by this group's UID (not a
// same-named predecessor) and carrying our managed-by label. The cache does not
// narrow Services, so this check is all that stands before an irreversible
// delete; the preconditions pin the object the decision was made about.
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

// reconcileNetworkPolicy keeps the group's egress policy in step with its
// online-mode, which is the one input that changes what a proxy may reach.
func (r *ProxyGroupReconciler) reconcileNetworkPolicy(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	online := proxyConfigValues(group).OnlineMode
	desired := podspec.BuildProxyNetworkPolicy(group.Spec.NetworkRef.Name, group, r.OperatorNamespace,
		online != nil && *online)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = desired.Labels
		policy.Spec = desired.Spec
		return controllerutil.SetControllerReference(group, policy, r.Scheme)
	})
	return err
}

func (r *ProxyGroupReconciler) reconcileConfigMap(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	data, err := yaml.Marshal(proxyConfigValues(group))
	if err != nil {
		return fmt.Errorf("marshal config.yaml for group %s: %w", group.Name, err)
	}
	return reconcileGroupConfigMap(ctx, r.Client, r.Scheme, group,
		podspec.GroupConfigMapName(group.Name, podspec.RoleProxy), data)
}

// proxyConfigValues builds the document reconcileConfigMap writes. The
// forwarding mode and ports are not in it; they live only in internal/render's
// critical layer.
//
// PlayerLimit is never nil, defaulting to podspec.DefaultPlayerLimit as the
// pod's environment does: render.Velocity refuses a missing limit, and every
// pod would crash-loop. OnlineMode is never nil either, defaulting to true like
// the CRD, so an unset spec renders an authenticating proxy.
func proxyConfigValues(group *spawneryv1alpha1.ProxyGroup) render.Values {
	var values render.Values
	limit := podspec.ProxyPlayerLimit(group)
	values.PlayerLimit = &limit
	onlineMode := true
	if cfg := group.Spec.Config; cfg != nil && cfg.OnlineMode != nil {
		onlineMode = *cfg.OnlineMode
	}
	values.OnlineMode = &onlineMode
	_, values.AcceptsTransfers = group.TransferForceAfter()
	if cfg := group.Spec.Config; cfg != nil && cfg.Motd != "" {
		motd := cfg.Motd
		values.Motd = &motd
	}
	return values
}

// setProxyPodsBlocked records why a proxy pod this group asked for does not
// exist, and reports whether Degraded turned True on this call.
//
// The same refusal leaves the condition untouched: the API server's message
// names the refused pod, and a fresh random name each pass would bump
// resourceVersion and re-enqueue the group in a tight loop. sameRefusal ignores
// that name; a different refusal under the same reason still replaces the
// message. The stored text stays the API server's own.
func setProxyPodsBlocked(group *spawneryv1alpha1.ProxyGroup, reason, message string) bool {
	// FindStatusCondition returns a pointer into the slice SetStatusCondition
	// writes through, so read the old verdict now.
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
// differing at most in the first quoted run, which the API server fills with
// the refused object's name:
//
//	pods "gateway-kt84" is forbidden: violates PodSecurity "baseline:latest": hostPort ...
//
// Unschedulable messages carry no quoted run and compare whole.
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

// proxyPodsAdmittedMessage claims no pod count: nothing on this path
// compares one.
const proxyPodsAdmittedMessage = "the API server refused no proxy pod of this group, " +
	"and none is reported unschedulable"

// reportBlockedProxies reports a proxy pod the scheduler cannot place (with
// hostPort at most one pod of a group fits per node) or one that crash-loops,
// naming the pod. It reports what the cluster said rather than predicting the
// scheduler. The all-clear fires an event on the flank too.
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

	// Scheduled and admitted but crash-looping. After the scheduling loop:
	// a pod that never landed cannot also be crashing.
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" {
				continue
			}
			// The waiting message only says "back-off ... restarting"; the last
			// termination says what killed it.
			detail := cs.State.Waiting.Message
			if last := cs.LastTerminationState.Terminated; last != nil {
				detail = fmt.Sprintf("exit %d (%s)", last.ExitCode, last.Reason)
			}
			message := fmt.Sprintf(
				"proxy pod %s is crash-looping: %s. Its container log has the cause; "+
					"a proxy that cannot serve its readiness probe stops on purpose so that this "+
					"is visible here rather than only as a pod that never turns ready",
				pods[i].Name, detail)
			if setProxyPodsBlocked(group, spawneryv1alpha1.ReasonCrashLoopBackoff, message) {
				r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, "ProxyPodBlocked",
					actionSyncStatus, "%s", eventNote("%s", message))
			}
			return
		}
	}
	// Only what the pass established: nothing refused, nothing unschedulable.
	// Not that the group is at size; a create may merely be deferred.
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
// readyReplicas counts Ready pods; connectedPlayers counts every pod.
func (r *ProxyGroupReconciler) setStatus(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service) {
	var ready int32
	var players int32
	for i := range pods {
		// Outside the readiness guard: a draining proxy is NotReady on purpose
		// and still carries players. Last-reported numbers, not measurements.
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

// readyHostIP is the node address of the first ready pod that has one: the
// readiness gate every strategy shares. test/e2e/expose_test.go rests on it.
func readyHostIP(pods []corev1.Pod) string {
	for i := range pods {
		if isPodReady(&pods[i]) && pods[i].Status.HostIP != "" {
			return pods[i].Status.HostIP
		}
	}
	return ""
}

// readyHostIPBindingPort is the node address of the first ready pod whose
// container declares hostPort. A group switched to HostPort keeps its old pods
// Ready until they are replaced, and their node is not listening on that port.
func readyHostIPBindingPort(pods []corev1.Pod, hostPort int32) string {
	// Zero would match every pod without a host port. The CRD forbids it; tests
	// do not pass through validation.
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

// allocatedNodePort reads the node port off the Service rather than the spec:
// once the Service is gone, the spec's port proves nothing.
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

// proxyAddress is where players connect, published only while a proxy is
// demonstrably serving; otherwise empty.
//
// NodePort and HostPort publish a ready pod's hostIP: the operator has no right
// to read Nodes and does not need one. LoadBalancer reads the Service, gated on
// readiness, which the Service knows nothing about. ClusterIP echoes the
// configured address, also gated on a ready pod although it reads nothing
// from it: status.address means "players can connect here now".
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
		// No port is appended: a client defaults to 25565. The Service is still
		// required; the published name routes to it.
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
		return ""
	}
}

// isPodReady reports what the kubelet says about the pod's readiness probe.
// For a proxy that is the whole ready gate: its agent turns the probe green
// only after processing its FullSync.
func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// exposeImplemented reports whether this operator has a branch for the
// strategy. A new strategy needs four edits nothing checks against each other:
// the CRD's enum, this list, and the switches in reconcileService and
// proxyAddress, whose default arms catch a half-done edit.
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

// groupsOfNetwork maps a Network event onto the ProxyGroups in its namespace
// that name it, like ServerGroupReconciler.groupsOfNetwork.
func (r *ProxyGroupReconciler) groupsOfNetwork(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &spawneryv1alpha1.ProxyGroupList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.NetworkRef.Name != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
		}})
	}
	return out
}

// groupsOnNode maps a Node event onto the ProxyGroups with pods on that node,
// like ServerGroupReconciler.groupsOnNode.
func (r *ProxyGroupReconciler) groupsOnNode(ctx context.Context, obj client.Object) []reconcile.Request {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleProxy,
	}); err != nil {
		// A map function cannot return an error; the resync is the fallback.
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
	// Divergence is reached on paths that return before reconcileReplicas,
	// including the first reconcile of a group already gone.
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
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.groupsOnNode)).
		// A group refused for its Network is no owner of it and would otherwise
		// wait out the resync.
		Watches(&spawneryv1alpha1.Network{},
			handler.EnqueueRequestsFromMapFunc(r.groupsOfNetwork)).
		Complete(r)
}
