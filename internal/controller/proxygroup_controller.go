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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/render"
)

// NewProxyName builds a unique proxy pod name below the group prefix. Same
// generator and same alphabet as NewServerName: a proxy has no CR of its own,
// so the pod name is the only handle anyone has on it, and it has to be as
// readable off a terminal as a server's.
func NewProxyName(group string) string { return NewServerName(group) }

// ProxyGroupReconciler keeps a proxy group at its replica count, keeps its
// Service in step, and publishes where players connect.
//
// Unlike ServerGroupReconciler it manages pods directly. Proxies are fungible:
// there is no per-proxy object, no state machine, and nothing to drain on the
// operator's side — a proxy's own agent moves its players, and that is
// milestone 4.
//
// It emits no Kubernetes events: this milestone's only visible signal is the
// Accepted condition, and events for pod creation or deletion are additional
// behaviour nothing here asks for. If a later milestone wants them, adding an
// EventRecorder field back alongside its call sites is a smaller change than
// carrying a field nothing writes to in the meantime.
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
		// proxy's own job and belongs to milestone 4.
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
	for i := len(pods) - 1; i >= int(group.Spec.Replicas); i-- {
		if err := r.Delete(ctx, &pods[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
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
// to a user. It carries only playerLimit and motd: online-mode, the
// forwarding mode and the ports are operationally critical and live in
// internal/render's critical layer and nowhere else, so there is exactly one
// place that can be wrong about any of them.
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
			Name:      podspec.GroupConfigMapName(group.Name),
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
// from whatever a user set in spec.config. A field spec.config leaves unset —
// spec.config itself being nil counts as every field unset — stays a nil
// pointer in the result rather than being defaulted to zero, so
// render.Values.RequirePlayerLimit can refuse a proxy that never got a
// capacity instead of silently starting one that reports slots=0 forever.
func proxyConfigValues(group *spawneryv1alpha1.ProxyGroup) render.Values {
	var values render.Values
	cfg := group.Spec.Config
	if cfg == nil {
		return values
	}
	if cfg.PlayerLimit != 0 {
		limit := cfg.PlayerLimit
		values.PlayerLimit = &limit
	}
	if cfg.Motd != "" {
		motd := cfg.Motd
		values.Motd = &motd
	}
	return values
}

// setStatus writes what is observably true of the group's pods.
func (r *ProxyGroupReconciler) setStatus(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod) {
	var ready int32
	var players int32
	for i := range pods {
		if !isPodReady(&pods[i]) {
			continue
		}
		ready++
		players += r.Agents.Lookup(string(pods[i].UID)).Players
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
