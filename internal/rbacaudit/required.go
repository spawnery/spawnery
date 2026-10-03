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

package rbacaudit

// RequiredCluster is the hand-maintained statement of what the operator does
// against the API in namespaces it does not know in advance. Deliberately not
// derived from the kubebuilder markers, so a new marker without an entry
// here turns the audit red. It catches drift between role and table, not a
// permission missing from both; that is the e2e test's job. A `get` entry
// names the call site even where today's cache serves the read locally.
var RequiredCluster = []Permission{
	// Events. create and patch but no update: client-go's event broadcaster only
	// ever calls Create and Patch on its sink. Only events.k8s.io here; the core
	// group's events are leader election's, in RequiredNamespaced.
	{Group: "events.k8s.io", Resource: "events", Verb: "create", Why: "Recorder.Eventf in every controller"},
	{Group: "events.k8s.io", Resource: "events", Verb: "patch", Why: "the recorder's event aggregation"},

	// Pods: Server and ProxyGroup controllers own their pods' life cycle.
	{Group: "", Resource: "pods", Verb: "get", Why: "ServerReconciler.fetchPod and ServerGroupReconciler.podFor"},
	{Group: "", Resource: "pods", Verb: "list", Why: "OrphanReconciler.Sweep, ProxyGroupReconciler.pods, and Store.namespacesMissingCA, which unions the namespaces holding a managed pod into a CA rotation's gate"},
	{Group: "", Resource: "pods", Verb: "watch", Why: "ServerReconciler and ProxyGroupReconciler both Owns(&corev1.Pod{})"},
	{Group: "", Resource: "pods", Verb: "create", Why: "ServerReconciler and ProxyGroupReconciler create pods from podspec"},
	{Group: "", Resource: "pods", Verb: "delete", Why: "the terminating decision, the orphan sweep, and ProxyGroupReconciler scaling down"},
	{Group: "", Resource: "pods", Verb: "patch", Why: "ServerReconciler.syncOccupiedLabel and ProxyGroupReconciler.syncOccupiedLabels patch the occupied label"},

	// PersistentVolumeClaims, one per persistent server. patch, not update, for
	// growClaim's one-field resize; get, list and watch back the restricted
	// cache. delete only on a plugin's request for an on-demand world.
	{Group: "", Resource: "persistentvolumeclaims", Verb: "get", Why: "the restricted cache over the world claims, and growClaim's read before it patches"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "list", Why: "the restricted cache over the world claims"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "watch", Why: "the restricted cache over the world claims"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "create", Why: "ServerReconciler creates a persistent server's claim before its pod"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "patch", Why: "ServerReconciler grows a world's claim when spec.storage.size grows; never update"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "delete", Why: "the agent writer deletes an on-demand member's world when a plugin asks for it; no other path deletes a claim"},

	// PodDisruptionBudgets, one per group, kept in step with the occupied count.
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "get", Why: "CreateOrUpdate in reconcilePDB and reconcileProxyPDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "list", Why: "ServerGroupReconciler and ProxyGroupReconciler both Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "watch", Why: "ServerGroupReconciler and ProxyGroupReconciler both Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "create", Why: "CreateOrUpdate in reconcilePDB and reconcileProxyPDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "update", Why: "CreateOrUpdate in reconcilePDB and reconcileProxyPDB"},

	// Namespace bootstrap: the CA ConfigMap and the agent ServiceAccounts. The
	// configmaps grant is shared with each group reconciler's own ConfigMap.
	{Group: "", Resource: "configmaps", Verb: "get", Why: "Bootstrapper.Ensure reads the CA ConfigMap, CreateOrUpdate in ServerGroupReconciler.reconcileConfigMap and ProxyGroupReconciler.reconcileConfigMap reads the group's, and Store.namespaceHasCA reads the CA ConfigMap again in every namespace with a Network while a CA rotation's gate is checking who has caught up"},
	{Group: "", Resource: "configmaps", Verb: "list", Why: "the restricted cache over the CA ConfigMaps and the group ConfigMaps, both of which carry the managed-by label the cache selects on"},
	{Group: "", Resource: "configmaps", Verb: "watch", Why: "the restricted cache over the CA ConfigMaps and the group ConfigMaps, both of which carry the managed-by label the cache selects on"},
	{Group: "", Resource: "configmaps", Verb: "create", Why: "Bootstrapper.Ensure creates the CA ConfigMap, and CreateOrUpdate in ServerGroupReconciler.reconcileConfigMap and ProxyGroupReconciler.reconcileConfigMap creates the group's"},
	{Group: "", Resource: "configmaps", Verb: "update", Why: "Bootstrapper.Ensure carries a changed CA forward, and the same two reconcileConfigMap calls carry a changed group config forward"},

	{Group: "", Resource: "serviceaccounts", Verb: "get", Why: "Bootstrapper.ensureServiceAccounts checks the server and proxy ServiceAccounts"},
	{Group: "", Resource: "serviceaccounts", Verb: "list", Why: "the restricted cache over the server and proxy ServiceAccounts"},
	{Group: "", Resource: "serviceaccounts", Verb: "watch", Why: "the restricted cache over the server and proxy ServiceAccounts"},
	{Group: "", Resource: "serviceaccounts", Verb: "create", Why: "Bootstrapper.ensureServiceAccounts creates the server and proxy ServiceAccounts"},

	// TokenReview is cluster-scoped; there is no namespaced variant.
	{Group: "authentication.k8s.io", Resource: "tokenreviews", Verb: "create",
		Why: "grpcauth.Authenticator.Authenticate checks every agent token"},

	// Pod metrics for /cloud status. A cluster may not serve metrics.k8s.io at
	// all; the answer then says usage is unavailable.
	{Group: "metrics.k8s.io", Resource: "pods", Verb: "list",
		Why: "netstatus.APIMetrics.PodUsage lists a namespace's pod metrics for /cloud status"},

	// The operator's own resources.
	{Group: "spawnery.cloud", Resource: "networks", Verb: "get", Why: "resolving networkRef"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "list", Why: "NetworkReconciler.namespaceOwner and its siblingNetworks mapper, and Store.namespacesMissingCA, which lists them again to find the namespaces a CA rotation's gate has to wait for"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "watch", Why: "NetworkReconciler For(&Network{}) and its Watches on the same type for siblings, and both group reconcilers Watches(&Network{}) so a refused group hears its Network come back"},
	// No status:get entries: Status().Update reads nothing first.
	{Group: "spawnery.cloud", Resource: "networks", Subresource: "status", Verb: "update", Why: "NetworkReconciler writes conditions and counts"},

	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "get", Why: "resolving groupRef"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "list", Why: "NetworkReconciler counts groups, and ServerGroupReconciler.groupsOfNetwork lists them again to find the ones a changed Network should wake"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "watch", Why: "ServerGroupReconciler For(&ServerGroup{})"},
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "status", Verb: "update", Why: "ServerGroupReconciler writes the aggregate and the conditions"},
	// SetControllerReference sets blockOwnerDeletion, which needs this.
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the owner references of Server and PodDisruptionBudget"},

	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "get", Why: "resolving a boost's group"},
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "list", Why: "ServerGroupReconciler adds live boosts to the floor"},
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "create", Why: "/cloud boost adds capacity for a while"},
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "delete", Why: "the orphan sweep removes expired boosts, and /cloud stop ends one early"},
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "watch", Why: "a boost created or deleted has to wake its group rather than wait out a resync"},

	{Group: "spawnery.cloud", Resource: "servers", Verb: "get", Why: "ServerReconciler.Reconcile"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "list", Why: "ServerGroupReconciler.collectViews and the orphan sweep"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "watch", Why: "ServerReconciler For(&Server{})"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "create", Why: "ServerGroupReconciler creates servers up to the lower bound"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "delete", Why: "scaling down, capping retained failures, the orphan sweep"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "update", Why: "setting and clearing the finalizer"},
	// A MergeFrom patch is its own verb; without it a rolling update never
	// nominates an old server to retire.
	{Group: "spawnery.cloud", Resource: "servers", Verb: "patch", Why: "ServerGroupReconciler.retireServer sets spec.retire; adoptServers stamps spec.podHash"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "status", Verb: "update", Why: "ServerReconciler writes phase, timestamps and conditions"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the pod owner references in podspec.BuildServerPod"},

	// The proxy layer's Service, one per ProxyGroup.
	{Group: "", Resource: "services", Verb: "get", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "list", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "watch", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "create", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "update", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "delete", Why: "reconcileService's HostPort branch, through deleteServiceIfOurs, removes the Service of a group switched to HostPort"},

	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "get", Why: "ProxyGroupReconciler.Reconcile and proxyreg.fallbacks"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "list", Why: "NetworkReconciler counts proxy groups, and ProxyGroupReconciler.groupsOfNetwork lists them again to find the ones a changed Network should wake"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "watch", Why: "ProxyGroupReconciler For(&ProxyGroup{})"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "status", Verb: "update", Why: "ProxyGroupReconciler writes replicas, address and conditions"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the pod, Service and PodDisruptionBudget owner references"},

	// Nodes: nodeDeparting checks whether a pod's node is cordoned or tainted.
	{Group: "", Resource: "nodes", Verb: "get", Why: "nodeDeparting resolves a pod's node name"},
	{Group: "", Resource: "nodes", Verb: "list", Why: "the restricted cache over Nodes"},
	{Group: "", Resource: "nodes", Verb: "watch", Why: "the restricted cache over Nodes"},

	// NetworkPolicies, cluster-wide because game namespaces are discovered at
	// runtime. No delete and no patch: the owner reference lets the garbage
	// collector remove them.
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "get", Why: "NetworkReconciler and ProxyGroupReconciler read their policies before they write"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "list", Why: "NetworkReconciler and ProxyGroupReconciler Owns(&networkingv1.NetworkPolicy{})"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "watch", Why: "NetworkReconciler and ProxyGroupReconciler Owns(&networkingv1.NetworkPolicy{})"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "create", Why: "the per-network backend policy and the per-group proxy egress policy"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "update", Why: "both policies are kept in step with the object that owns them"},
}

// RequiredNamespaced is checked against the Role in the operator's own
// namespace. No list or watch on secrets, so certs.Store runs uncached rather
// than needing an informer over every Secret; TestTheAuthorizerActuallyDenies
// keeps it that way.
var RequiredNamespaced = []Permission{
	{Group: "", Resource: "secrets", Verb: "get", Why: "certs.Store.Ensure reads the TLS bundle"},
	{Group: "", Resource: "secrets", Verb: "create", Why: "certs.Store.Ensure creates it on first start"},
	{Group: "", Resource: "secrets", Verb: "update", Why: "certs.Store.Ensure renews the serving certificate"},

	{Group: "coordination.k8s.io", Resource: "leases", Verb: "create", Why: "leader election on startup"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "get", Why: "leader election renews the lock"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "update", Why: "leader election renews the lock"},

	// Core events, for leader election's lock on a Lease in this namespace only.
	{Group: "", Resource: "events", Verb: "create", Why: "leader election's resource lock records elections"},
	{Group: "", Resource: "events", Verb: "patch", Why: "the leader election recorder's event aggregation"},
}

// RequiredNetworkNamespace is granted per Network namespace by
// config/rbac/forwarding-secret-reader.yaml, not by the ClusterRole: a
// cluster-wide secrets/get would read every Secret in the cluster, and
// Secret names are visible in pod specs anyway. That manifest is
// hand-written, so this comparison is the only check on it.
var RequiredNetworkNamespace = []Permission{
	{Group: "", Resource: "secrets", Verb: "get", Why: "readForwardingSecret digests the forwarding secret to detect a rotation"},
}
