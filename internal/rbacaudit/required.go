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

package rbacaudit

// RequiredCluster is the hand-maintained statement of what the operator's code
// does against the Kubernetes API in namespaces it does not know in advance —
// wherever a Network puts its game servers. It is deliberately not derived from
// the kubebuilder markers: a derived table would only prove that the generated
// role grants what the generated role grants.
//
// Adding a marker without adding an entry here turns the audit red. That is
// the point — it forces a moment of thought about whether the new permission
// is really needed.
//
// Note the limit of this table: it catches drift between role and table, not
// a permission missing from both. Only the operator actually running under
// this ServiceAccount can prove completeness, which is what the cluster-level
// end-to-end test is for.
//
// A `get` entry sometimes names a call site whose read never crosses the wire
// — `pods`, `poddisruptionbudgets` and `services` are all in the manager's
// cache, so the `Get` inside `CreateOrUpdate` or a plain fetch is served from
// the informer's local store rather than a live API call. The entry is kept
// anyway: it records what the code calls, not what today's cache
// configuration happens to make free, so swapping that call site onto an
// uncached client later is not also an RBAC change nobody remembered to make.
var RequiredCluster = []Permission{
	// Events — the recorder writes them for every phase change and every
	// warning, and patches them when it aggregates repeats.
	{Group: "", Resource: "events", Verb: "create", Why: "Recorder.Eventf in every controller"},
	{Group: "", Resource: "events", Verb: "patch", Why: "the recorder's event aggregation"},

	// Pods — the Server controller owns a game server pod's whole life cycle;
	// since this milestone ProxyGroupReconciler owns a proxy pod's the same way.
	{Group: "", Resource: "pods", Verb: "get", Why: "ServerReconciler.fetchPod and ServerGroupReconciler.podFor"},
	{Group: "", Resource: "pods", Verb: "list", Why: "OrphanReconciler.Sweep and ProxyGroupReconciler.pods"},
	{Group: "", Resource: "pods", Verb: "watch", Why: "ServerReconciler and ProxyGroupReconciler both Owns(&corev1.Pod{})"},
	{Group: "", Resource: "pods", Verb: "create", Why: "ServerReconciler and ProxyGroupReconciler create pods from podspec"},
	{Group: "", Resource: "pods", Verb: "delete", Why: "the terminating decision, the orphan sweep, and ProxyGroupReconciler scaling down"},
	{Group: "", Resource: "pods", Verb: "patch", Why: "syncOccupiedLabel patches the occupied label"},

	// PodDisruptionBudgets — one per group, kept in step with the occupied count.
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "get", Why: "CreateOrUpdate in reconcilePDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "list", Why: "ServerGroupReconciler Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "watch", Why: "ServerGroupReconciler Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "create", Why: "CreateOrUpdate in reconcilePDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "update", Why: "CreateOrUpdate in reconcilePDB"},

	// Namespace bootstrap — Bootstrapper.Ensure keeps the CA ConfigMap and
	// the server and proxy ServiceAccounts current in every namespace that
	// runs pods.
	{Group: "", Resource: "configmaps", Verb: "get", Why: "Bootstrapper.Ensure reads the CA ConfigMap"},
	{Group: "", Resource: "configmaps", Verb: "list", Why: "the restricted cache over the CA ConfigMaps"},
	{Group: "", Resource: "configmaps", Verb: "watch", Why: "the restricted cache over the CA ConfigMaps"},
	{Group: "", Resource: "configmaps", Verb: "create", Why: "Bootstrapper.Ensure creates the CA ConfigMap"},
	{Group: "", Resource: "configmaps", Verb: "update", Why: "Bootstrapper.Ensure carries a changed CA forward"},

	{Group: "", Resource: "serviceaccounts", Verb: "get", Why: "Bootstrapper.ensureServiceAccounts checks the server and proxy ServiceAccounts"},
	{Group: "", Resource: "serviceaccounts", Verb: "list", Why: "the restricted cache over the server and proxy ServiceAccounts"},
	{Group: "", Resource: "serviceaccounts", Verb: "watch", Why: "the restricted cache over the server and proxy ServiceAccounts"},
	{Group: "", Resource: "serviceaccounts", Verb: "create", Why: "Bootstrapper.ensureServiceAccounts creates the server and proxy ServiceAccounts"},

	// Every agent token is checked against the real authenticator of the API
	// server. TokenReview is cluster-scoped, so there is no namespaced variant
	// of this right to fall back to.
	{Group: "authentication.k8s.io", Resource: "tokenreviews", Verb: "create",
		Why: "grpcauth.Authenticator.Authenticate checks every agent token"},

	// The operator's own resources.
	{Group: "spawnery.cloud", Resource: "networks", Verb: "get", Why: "resolving networkRef"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "list", Why: "NetworkReconciler.namespaceOwner"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "watch", Why: "NetworkReconciler For(&Network{})"},
	// No entry for networks/status:get. Status().Update issues a PUT against
	// the status subresource and reads nothing first; the status itself is read
	// off the object returned by a plain Get on the resource. The same holds for
	// the other two /status subresources below.
	{Group: "spawnery.cloud", Resource: "networks", Subresource: "status", Verb: "update", Why: "NetworkReconciler writes conditions and counts"},

	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "get", Why: "resolving groupRef"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "list", Why: "NetworkReconciler counts groups"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "watch", Why: "ServerGroupReconciler For(&ServerGroup{})"},
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "status", Verb: "update", Why: "ServerGroupReconciler writes the aggregate and the conditions"},
	// Needed for the same reason as servers/finalizers below: createServer and
	// reconcilePDB both call controllerutil.SetControllerReference with the
	// group as owner, and that sets blockOwnerDeletion on the reference.
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the owner references of Server and PodDisruptionBudget"},

	{Group: "spawnery.cloud", Resource: "servers", Verb: "get", Why: "ServerReconciler.Reconcile"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "list", Why: "ServerGroupReconciler.collectViews and the orphan sweep"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "watch", Why: "ServerReconciler For(&Server{})"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "create", Why: "ServerGroupReconciler creates servers up to the lower bound"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "delete", Why: "scaling down, capping retained failures, the orphan sweep"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "update", Why: "setting and clearing the finalizer"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "status", Verb: "update", Why: "ServerReconciler writes phase, timestamps and conditions"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the pod owner references in podspec.BuildServerPod"},

	// The proxy layer's Service. One per ProxyGroup, and the only way a player
	// reaches a proxy at all.
	{Group: "", Resource: "services", Verb: "get", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "list", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "watch", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "create", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "update", Why: "CreateOrUpdate in ProxyGroupReconciler"},

	// Two things fetch a single ProxyGroup as of this milestone: the
	// reconciler itself, and the fan-out reading a group's fallback list.
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "get", Why: "ProxyGroupReconciler.Reconcile and proxyreg.fallbacks"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "list", Why: "NetworkReconciler counts proxy groups"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "watch", Why: "ProxyGroupReconciler For(&ProxyGroup{})"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "status", Verb: "update", Why: "ProxyGroupReconciler writes replicas, address and conditions"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the pod and Service owner references"},
}

// RequiredNamespaced is what the operator does in its own namespace only, and
// is checked against the generated Role rather than the ClusterRole. Both
// entries were cluster-wide before milestone 2a: the lease because nobody had
// split the table yet, the secret because it did not exist yet — and granting a
// cluster-wide write on secrets would have been the wrong signal in exactly the
// milestone that introduces the TLS channel.
//
// Note which verbs are absent: no list and no watch on secrets. certs.Store
// therefore runs on an uncached client on purpose, because a cached Secret would
// require an informer over every Secret in the namespace. Adding those verbs to
// make caching work would widen this beyond what the design intends;
// TestTheAuthorizerActuallyDenies insists they stay out.
var RequiredNamespaced = []Permission{
	{Group: "", Resource: "secrets", Verb: "get", Why: "certs.Store.Ensure reads the TLS bundle"},
	{Group: "", Resource: "secrets", Verb: "create", Why: "certs.Store.Ensure creates it on first start"},
	{Group: "", Resource: "secrets", Verb: "update", Why: "certs.Store.Ensure renews the serving certificate"},

	{Group: "coordination.k8s.io", Resource: "leases", Verb: "create", Why: "leader election on startup"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "get", Why: "leader election renews the lock"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "update", Why: "leader election renews the lock"},
}
