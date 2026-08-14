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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
// It emits exactly one Kubernetes event, and only from that deadline. Pod
// creation and ordinary deletion stay silent: they are recorded on the
// objects themselves and an event would say nothing the group's status does
// not. A drain that ran out of time is different in kind — it is the only
// thing in this milestone that disconnects a player, and nothing else on the
// object would ever say it happened.
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
	// Recorder announces the one thing here a user cannot see any other way:
	// a drain that hit its deadline with players still on the proxy. See the
	// type comment for why nothing else is announced.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update

// Reconcile brings one ProxyGroup in line with its spec.
func (r *ProxyGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	group := &spawneryv1alpha1.ProxyGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// The pods and the Service are owned by this object, so the API server
		// removes them. There is nothing to drain here: moving players is the
		// proxy's own job, and making deletion wait for it belongs to a later
		// task.
		return ctrl.Result{}, nil
	}

	network := &spawneryv1alpha1.Network{}
	key := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: group.Namespace}
	switch err := r.Get(ctx, key, network); {
	case apierrors.IsNotFound(err):
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonNetworkNotFound,
			fmt.Sprintf("Network %q does not exist", group.Spec.NetworkRef.Name))
		return ctrl.Result{RequeueAfter: networkRetryInterval}, r.writeStatus(ctx, group)
	case err != nil:
		return ctrl.Result{}, err
	}
	if !meta.IsStatusConditionTrue(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonNetworkNotAccepted,
			networkNotAcceptedMessage(network))
		return ctrl.Result{RequeueAfter: networkRetryInterval}, r.writeStatus(ctx, group)
	}

	// Milestone 6 owns the other two strategies. Refusing is the honest
	// version: a LoadBalancer branch written now would reach milestone 6 having
	// never run, because the local flow cannot produce a cluster that would
	// exercise it. A refusal on the object is also the only form a user can
	// see — a group that silently does nothing looks like a dead operator.
	if group.Spec.Expose.Type != spawneryv1alpha1.ExposeNodePort {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonExposeNotImplemented,
			fmt.Sprintf("expose.type %s arrives with milestone 6; only NodePort is implemented",
				group.Spec.Expose.Type))
		return ctrl.Result{}, r.writeStatus(ctx, group)
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

	if err := r.Bootstrap.Ensure(ctx, group.Namespace); err != nil {
		return ctrl.Result{}, err
	}
	// Rendered here, beside Bootstrap.Ensure and before reconcileReplicas can
	// create the first proxy pod: that pod's projected volume names this
	// ConfigMap by group (podspec.GroupConfigMapName), and returning on error
	// stops the reconcile before reconcileReplicas runs — the guarantee is
	// the early return, not the order these lines happen to be written in.
	if err := r.reconcileConfigMap(ctx, group); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileService(ctx, group); err != nil {
		return ctrl.Result{}, err
	}

	pods, err := r.pods(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileReplicas(ctx, network, group, pods); err != nil {
		return ctrl.Result{}, err
	}

	// Re-read after the changes, so the status describes what is there rather
	// than what was there when the reconcile started.
	pods, err = r.pods(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.setStatus(group, pods)
	return ctrl.Result{RequeueAfter: resyncInterval}, r.writeStatus(ctx, group)
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
	return live, nil
}

// reconcileReplicas creates or removes pods until the count matches the spec.
// Scale-down takes the newest first: an older proxy has had longer to collect
// players, and this milestone has no way to move them.
func (r *ProxyGroupReconciler) reconcileReplicas(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) error {
	for i := int32(len(pods)); i < group.Spec.Replicas; i++ {
		pod, err := podspec.BuildProxyPod(network, group, NewProxyName(group.Name), r.AgentEndpoint)
		if err != nil {
			return err
		}
		if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	// The desired readiness is derived, not remembered: this loop already
	// knows which pods are surplus, so it asserts the answer for every pod on
	// every pass. An operator restart recomputes the same thing, and a
	// cancelled scale-down corrects itself without anything to clean up.
	for i := range pods {
		surplus := i >= int(group.Spec.Replicas)
		if err := r.Proxies.SetReady(ctx, string(pods[i].UID), !surplus); err != nil {
			return err
		}
		if err := r.markDraining(ctx, &pods[i], surplus); err != nil {
			return err
		}
	}

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
	// still up. A count we cannot trust is treated as occupied — the single
	// occupancy rule of this repository, the one candidates.go's isOccupied
	// states and the Server controller obeys. It matters more here than the
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
	// isOccupied has a third term this deliberately does not model:
	// sessionsGone, which overrides even a non-zero count, because a pod that
	// reached a terminal state took its sessions down with it. A crashed
	// surplus proxy therefore waits out its full deadline here rather than
	// going immediately, and is then announced as losing players who were
	// disconnected by the crash. That is wasteful and the event overstates,
	// but both err towards keeping a pod that might still have someone on it,
	// and the wait is bounded. Reading pod state to decide it is a wider
	// change than this task, which only ever asks the registry.
	for i := len(pods) - 1; i >= int(group.Spec.Replicas); i-- {
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
		case players == 0 && !snap.PlayersStale && snap.Connected:
			// Known empty: nobody is on it, so removing it costs nothing.
		case expired:
			// The one path in this milestone that disconnects anybody. It is
			// configured rather than accidental, so it says so loudly and
			// names the cost.
			r.Recorder.Eventf(group, corev1.EventTypeWarning, "ProxyDrainTimeout",
				"deleting proxy %s after %s with %d player(s) still connected",
				pod.Name, group.DrainTimeout(), players)
		default:
			// Still draining. Nothing to do; the next pass looks again.
			continue
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
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

// reconcileService keeps the NodePort Service in step with the group.
func (r *ProxyGroupReconciler) reconcileService(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: group.Name, Namespace: group.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		svc.Spec.Type = corev1.ServiceTypeNodePort
		// Local, not the Cluster default, for the same reason
		// LoadBalancerSpec.ExternalTrafficPolicy defaults to Local: the default
		// SNATs, so Velocity would never see a player's real IP, and bans and
		// rate limits depend on it. The consequence is the trade-off this makes:
		// a client that reaches a node running no proxy pod for this group gets
		// no answer at all, rather than being routed to one that does. That is
		// consistent with proxyAddress below only ever publishing the hostIP of
		// a node that demonstrably runs a ready proxy — a client dialing the
		// published address never hits the empty case.
		svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		// The selector must pin the role as well as the group: without it the
		// Service would also select any server pod that happened to share the
		// group name, and players would land on a backend directly.
		svc.Spec.Selector = podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       podspec.MinecraftPortName,
			Port:       podspec.MinecraftPort,
			TargetPort: intstr.FromString(podspec.MinecraftPortName),
			NodePort:   group.Spec.Expose.NodePort.Port,
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(group, svc, r.Scheme)
	})
	return err
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

// setStatus writes what is observably true of the group's pods.
//
// The two counts deliberately answer different questions about the same pods.
// readyReplicas is about the ready gate, so it counts only pods the kubelet
// calls Ready. connectedPlayers is about people, so it counts every pod in the
// group, ready or not — the CRD calls it "the sum of players across all
// proxies" and it is a printed column.
func (r *ProxyGroupReconciler) setStatus(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod) {
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
	group.Status.Address = proxyAddress(pods, group.Spec.Expose.NodePort.Port)
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

// proxyAddress is where players connect.
//
// With NodePort that is a node's address plus the node port, and the operator
// has no right to read Node objects — nor does it need one: hostIP on a ready
// proxy pod is the address of a node that demonstrably has a proxy on it, and
// the pod is already watched. Granting a cluster-wide node read for a status
// string would be the same trade the bootstrapper refused when it declined the
// update verb on ServiceAccounts to restore a cosmetic label.
//
// Empty while nothing is ready, which is the truthful answer: there is nowhere
// to connect yet, and printing a node address for a proxy that is not serving
// would send players at a closed port.
func proxyAddress(pods []corev1.Pod, nodePort int32) string {
	for i := range pods {
		if isPodReady(&pods[i]) && pods[i].Status.HostIP != "" {
			return fmt.Sprintf("%s:%d", pods[i].Status.HostIP, nodePort)
		}
	}
	return ""
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

// SetupWithManager registers the controller.
func (r *ProxyGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.ProxyGroup{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
