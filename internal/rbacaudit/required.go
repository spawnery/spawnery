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
// A marker that produces no rule at all: controller-gen ignores a
// +kubebuilder:rbac marker that sits inside a doc comment rather than
// immediately before the declaration it applies to -- no rule, no error, no
// diagnostic. Task 10 walked into it twice; the first attempt at `secrets` and
// `tokenreviews` produced nothing whatsoever. After adding a marker, diff
// config/rbac/role.yaml rather than watching `make manifests` go green.

var RequiredCluster = []Permission{
	// Events — the recorder writes them for every phase change and every
	// warning, and patches them when it aggregates repeats.
	//
	// create;patch and no update, read out of client-go rather than inferred
	// from a green run, which is the only reason to believe it. In v0.36.0 the
	// broadcaster's recordEvent (tools/events/event_broadcaster.go:230-273)
	// calls exactly two methods on its sink: Patch when the event is a series,
	// Create otherwise or when the patch found nothing to patch. EventSink's
	// Update is declared in the same package (tools/events/interfaces.go:71)
	// and called from nowhere in it, and the deprecated recorder
	// (tools/record/event.go:330-341) has the identical shape. So this pair is
	// neither short a verb the library will reach for nor carrying one it will
	// not -- a statement about the library's source, which a `make e2e` PASS
	// could not have made.
	//
	// Only events.k8s.io is here, and only because these events regard objects
	// in namespaces the operator does not know in advance: every controller in
	// internal/controller writes through k8s.io/client-go/tools/events, whose
	// sink is the events.k8s.io/v1 client, about Servers and pods wherever a
	// Network put them. The core group's own grant is real and still needed —
	// controller-runtime's leader election builds its resource lock with the
	// deprecated GetEventRecorderFor, which writes core events — but that lock
	// is a Lease in the operator's own namespace, so its right is namespaced
	// and lives in RequiredNamespaced below.
	//
	// Nothing in the test suite can catch either of these missing. envtest
	// grants the test client everything, so an event the operator is not
	// allowed to write is only visible under the real ServiceAccount — which
	// is what the cluster-level end-to-end test is for.
	{Group: "events.k8s.io", Resource: "events", Verb: "create", Why: "Recorder.Eventf in every controller"},
	{Group: "events.k8s.io", Resource: "events", Verb: "patch", Why: "the recorder's event aggregation"},

	// Pods — the Server controller owns a game server pod's whole life cycle;
	// since this milestone ProxyGroupReconciler owns a proxy pod's the same way.
	{Group: "", Resource: "pods", Verb: "get", Why: "ServerReconciler.fetchPod and ServerGroupReconciler.podFor"},
	{Group: "", Resource: "pods", Verb: "list", Why: "OrphanReconciler.Sweep, ProxyGroupReconciler.pods, and Store.namespacesMissingCA, which unions the namespaces holding a managed pod into a CA rotation's gate"},
	{Group: "", Resource: "pods", Verb: "watch", Why: "ServerReconciler and ProxyGroupReconciler both Owns(&corev1.Pod{})"},
	{Group: "", Resource: "pods", Verb: "create", Why: "ServerReconciler and ProxyGroupReconciler create pods from podspec"},
	{Group: "", Resource: "pods", Verb: "delete", Why: "the terminating decision, the orphan sweep, and ProxyGroupReconciler scaling down"},
	{Group: "", Resource: "pods", Verb: "patch", Why: "ServerReconciler.syncOccupiedLabel and ProxyGroupReconciler.syncOccupiedLabels patch the occupied label"},

	// PersistentVolumeClaims — one per persistent server, created before the
	// pod that mounts it.
	//
	// Note what is still absent, because the absence is the safety property
	// and not an oversight: no delete. The operator never removes a world — a
	// claim carries no owner reference and outlives its server, its group, and
	// the operator who deleted the wrong object, so the right to delete one is
	// a right nothing here wants. Reclaiming a world is a deliberate human
	// act, done with kubectl against a documented runbook.
	//
	// patch is 5b's one concession, and deliberately not update: growClaim
	// raises spec.resources.requests.storage to match spec.storage.size and
	// never lowers it, one field at a time, which is what patch is for.
	// update would replace the whole claim for that one field.
	//
	// get, list and watch serve the restricted cache cmd/spawnery-operator
	// declares over claims, and also back growClaim's own read of the claim it
	// patches. BuildDataClaim still renders the claim it creates from the
	// group's spec.storage without consulting the API. They are the read half
	// of a kind the manager caches, kept together so the first read is a code
	// change and not also an RBAC one.
	{Group: "", Resource: "persistentvolumeclaims", Verb: "get", Why: "the restricted cache over the world claims, and growClaim's read before it patches"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "list", Why: "the restricted cache over the world claims"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "watch", Why: "the restricted cache over the world claims"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "create", Why: "ServerReconciler creates a persistent server's claim before its pod"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "patch", Why: "ServerReconciler grows a world's claim when spec.storage.size grows; never update, never delete"},

	// PodDisruptionBudgets — one per group (ServerGroup and ProxyGroup each own
	// one, distinguished by podspec.GroupPDBName's role suffix), kept in step
	// with the occupied count.
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "get", Why: "CreateOrUpdate in reconcilePDB and reconcileProxyPDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "list", Why: "ServerGroupReconciler and ProxyGroupReconciler both Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "watch", Why: "ServerGroupReconciler and ProxyGroupReconciler both Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "create", Why: "CreateOrUpdate in reconcilePDB and reconcileProxyPDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "update", Why: "CreateOrUpdate in reconcilePDB and reconcileProxyPDB"},

	// Namespace bootstrap — Bootstrapper.Ensure keeps the CA ConfigMap and
	// the server and proxy ServiceAccounts current in every namespace that
	// runs pods. The configmaps grant is shared: each group reconciler writes
	// its own ConfigMap under the same verbs, which is why the Why lines below
	// name both consumers rather than the bootstrapper alone.
	{Group: "", Resource: "configmaps", Verb: "get", Why: "Bootstrapper.Ensure reads the CA ConfigMap, CreateOrUpdate in ServerGroupReconciler.reconcileConfigMap and ProxyGroupReconciler.reconcileConfigMap reads the group's, and Store.namespaceHasCA reads the CA ConfigMap again in every namespace with a Network while a CA rotation's gate is checking who has caught up"},
	{Group: "", Resource: "configmaps", Verb: "list", Why: "the restricted cache over the CA ConfigMaps and the group ConfigMaps, both of which carry the managed-by label the cache selects on"},
	{Group: "", Resource: "configmaps", Verb: "watch", Why: "the restricted cache over the CA ConfigMaps and the group ConfigMaps, both of which carry the managed-by label the cache selects on"},
	{Group: "", Resource: "configmaps", Verb: "create", Why: "Bootstrapper.Ensure creates the CA ConfigMap, and CreateOrUpdate in ServerGroupReconciler.reconcileConfigMap and ProxyGroupReconciler.reconcileConfigMap creates the group's"},
	{Group: "", Resource: "configmaps", Verb: "update", Why: "Bootstrapper.Ensure carries a changed CA forward, and the same two reconcileConfigMap calls carry a changed group config forward"},

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
	{Group: "spawnery.cloud", Resource: "networks", Verb: "list", Why: "NetworkReconciler.namespaceOwner and its siblingNetworks mapper, and Store.namespacesMissingCA, which lists them again to find the namespaces a CA rotation's gate has to wait for"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "watch", Why: "NetworkReconciler For(&Network{}) and its Watches on the same type for siblings, and both group reconcilers Watches(&Network{}) so a refused group hears its Network come back"},
	// No entry for networks/status:get. Status().Update issues a PUT against
	// the status subresource and reads nothing first; the status itself is read
	// off the object returned by a plain Get on the resource. The same holds for
	// the other two /status subresources below.
	{Group: "spawnery.cloud", Resource: "networks", Subresource: "status", Verb: "update", Why: "NetworkReconciler writes conditions and counts"},

	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "get", Why: "resolving groupRef"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "list", Why: "NetworkReconciler counts groups, and ServerGroupReconciler.groupsOfNetwork lists them again to find the ones a changed Network should wake"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "watch", Why: "ServerGroupReconciler For(&ServerGroup{})"},
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "status", Verb: "update", Why: "ServerGroupReconciler writes the aggregate and the conditions"},
	// Needed for the same reason as servers/finalizers below: createServer and
	// reconcilePDB both call controllerutil.SetControllerReference with the
	// group as owner, and that sets blockOwnerDeletion on the reference.
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
	// Not covered by "update" above: a MergeFrom patch is its own verb to the
	// API server. Without this the operator can create and delete servers but
	// never nominate one, so a ServerGroup rolling update stops after the new
	// generation is up and the old one is never asked to go. Measured on a
	// live cluster 2026-08-25.
	{Group: "spawnery.cloud", Resource: "servers", Verb: "patch", Why: "ServerGroupReconciler.retireServer sets spec.retire; adoptServers stamps spec.podHash"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "status", Verb: "update", Why: "ServerReconciler writes phase, timestamps and conditions"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the pod owner references in podspec.BuildServerPod"},

	// The proxy layer's Service. One per ProxyGroup, and the only way a player
	// reaches a proxy at all.
	{Group: "", Resource: "services", Verb: "get", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "list", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "watch", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "create", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "update", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "delete", Why: "reconcileService's HostPort branch, through deleteServiceIfOurs, removes the Service of a group switched to HostPort"},

	// Two things fetch a single ProxyGroup as of this milestone: the
	// reconciler itself, and the fan-out reading a group's fallback list.
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "get", Why: "ProxyGroupReconciler.Reconcile and proxyreg.fallbacks"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "list", Why: "NetworkReconciler counts proxy groups, and ProxyGroupReconciler.groupsOfNetwork lists them again to find the ones a changed Network should wake"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "watch", Why: "ProxyGroupReconciler For(&ProxyGroup{})"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "status", Verb: "update", Why: "ProxyGroupReconciler writes replicas, address and conditions"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the pod, Service and PodDisruptionBudget owner references"},

	// Nodes — nodeDeparting resolves a pod's node to ask IsDeparting whether it
	// is cordoned or tainted to repel, so a group can empty a pod off a node
	// leaving service before somebody else moves it the hard way.
	{Group: "", Resource: "nodes", Verb: "get", Why: "nodeDeparting resolves a pod's node name"},
	{Group: "", Resource: "nodes", Verb: "list", Why: "the restricted cache over Nodes"},
	{Group: "", Resource: "nodes", Verb: "watch", Why: "the restricted cache over Nodes"},

	// NetworkPolicies — one per accepted Network, written into that Network's
	// own namespace, which is why this is cluster-wide: game namespaces are
	// discovered at runtime and no install-time list of them exists.
	//
	// No delete and no patch, deliberately, and the omission is enforced
	// rather than merely documented: the policy carries an owner reference to
	// its Network, so the garbage collector removes it. A delete marker added
	// later turns this suite red in both directions before it can ship, the
	// same way the persistentvolumeclaims grant above works.
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "get", Why: "NetworkReconciler.reconcileNetworkPolicy reads before it writes"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "list", Why: "NetworkReconciler Owns(&networkingv1.NetworkPolicy{})"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "watch", Why: "NetworkReconciler Owns(&networkingv1.NetworkPolicy{})"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "create", Why: "NetworkReconciler.reconcileNetworkPolicy creates the per-network policy"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "update", Why: "NetworkReconciler.reconcileNetworkPolicy keeps it in step with the Network"},
}

// RequiredNamespaced is what the operator does in its own namespace only, and
// is checked against the generated Role rather than the ClusterRole. The lease
// and the secret were both cluster-wide before milestone 2a: the lease because
// nobody had split the table yet, the secret because it did not exist yet — and
// granting a cluster-wide write on secrets would have been the wrong signal in
// exactly the milestone that introduces the TLS channel.
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

	// The core group's events, which are the leader-election lock's and
	// nobody else's. This was cluster-wide until milestone 6e's final review:
	// before the migration off tools/record every controller wrote core
	// events, so the width was right; afterwards the controllers write only to
	// events.k8s.io and the sole remaining consumer is a lock on a Lease in
	// this one namespace. A cluster-wide grant that no cluster-wide caller
	// needs is the kind that outlives its reason quietly.
	{Group: "", Resource: "events", Verb: "create", Why: "leader election's resource lock records elections"},
	{Group: "", Resource: "events", Verb: "patch", Why: "the leader election recorder's event aggregation"},
}

// RequiredNetworkNamespace is what the operator needs in every namespace that
// holds a Network. It is granted by config/rbac/forwarding-secret-reader.yaml
// rather than by the ClusterRole, and an administrator applies it per
// namespace.
//
// Not one line in RequiredCluster, which is what it would have been: a
// cluster-wide secrets/get makes the operator's ServiceAccount a reader of
// every Secret in the cluster. "get without list means you must know the name"
// carries less than it sounds like — Secret names are visible in the pod specs
// this operator already lists. TestTheAuthorizerActuallyDenies probes
// secrets/get in a foreign namespace and requires a denial; that probe is right
// and stays.
//
// Unlike the other two tables this one is compared against a hand-written
// manifest rather than a generated one, so both directions of the comparison
// are the only thing checking that file at all.
var RequiredNetworkNamespace = []Permission{
	{Group: "", Resource: "secrets", Verb: "get", Why: "readForwardingSecret digests the forwarding secret to detect a rotation"},
}
