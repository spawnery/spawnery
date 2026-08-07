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

// Required is the hand-maintained statement of what the operator's code
// actually does against the Kubernetes API. It is deliberately not derived
// from the kubebuilder markers: a derived table would only prove that the
// generated role grants what the generated role grants.
//
// Adding a marker without adding an entry here turns the audit red. That is
// the point — it forces a moment of thought about whether the new permission
// is really needed.
//
// Note the limit of this table: it catches drift between role and table, not
// a permission missing from both. Only the operator actually running under
// this ServiceAccount can prove completeness, which is what the cluster-level
// end-to-end test is for.
var Required = []Permission{
	// Events — the recorder writes them for every phase change and every
	// warning, and patches them when it aggregates repeats.
	{Group: "", Resource: "events", Verb: "create", Why: "Recorder.Eventf in allen Controllern"},
	{Group: "", Resource: "events", Verb: "patch", Why: "Event-Aggregation des Recorders"},

	// Pods — the Server controller owns their whole life cycle.
	{Group: "", Resource: "pods", Verb: "get", Why: "ServerReconciler.fetchPod und ServerGroupReconciler.podFor"},
	{Group: "", Resource: "pods", Verb: "list", Why: "OrphanReconciler.Sweep"},
	{Group: "", Resource: "pods", Verb: "watch", Why: "ServerReconciler Owns(&corev1.Pod{})"},
	{Group: "", Resource: "pods", Verb: "create", Why: "ServerReconciler erzeugt den Pod aus podspec"},
	{Group: "", Resource: "pods", Verb: "delete", Why: "Terminating-Entscheidung und Verwaisten-Abgleich"},
	{Group: "", Resource: "pods", Verb: "patch", Why: "syncOccupiedLabel patcht das Occupied-Label"},

	// PodDisruptionBudgets — one per group, kept in step with the occupied count.
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "get", Why: "CreateOrUpdate in reconcilePDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "list", Why: "ServerGroupReconciler Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "watch", Why: "ServerGroupReconciler Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "create", Why: "CreateOrUpdate in reconcilePDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "update", Why: "CreateOrUpdate in reconcilePDB"},

	// Leader election locks on a Lease in the operator's own namespace.
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "create", Why: "Leader-Election beim Start"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "get", Why: "Leader-Election erneuert die Sperre"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "update", Why: "Leader-Election erneuert die Sperre"},

	// The operator's own resources.
	{Group: "spawnery.cloud", Resource: "networks", Verb: "get", Why: "Auflösen von networkRef"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "list", Why: "NetworkReconciler.namespaceOwner"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "watch", Why: "NetworkReconciler For(&Network{})"},
	// No entry for networks/status:get. Status().Update issues a PUT against
	// the status subresource and reads nothing first; the status itself is read
	// off the object returned by a plain Get on the resource. The same holds for
	// the other two /status subresources below.
	{Group: "spawnery.cloud", Resource: "networks", Subresource: "status", Verb: "update", Why: "NetworkReconciler schreibt Conditions und Zähler"},

	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "get", Why: "Auflösen von groupRef"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "list", Why: "NetworkReconciler zählt Gruppen"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "watch", Why: "ServerGroupReconciler For(&ServerGroup{})"},
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "status", Verb: "update", Why: "ServerGroupReconciler schreibt Aggregation und Conditions"},
	// Needed for the same reason as servers/finalizers below: createServer and
	// reconcilePDB both call controllerutil.SetControllerReference with the
	// group as owner, and that sets blockOwnerDeletion on the reference.
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion auf den OwnerReferences von Server und PodDisruptionBudget"},

	{Group: "spawnery.cloud", Resource: "servers", Verb: "get", Why: "ServerReconciler.Reconcile"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "list", Why: "ServerGroupReconciler.collectViews und Verwaisten-Abgleich"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "watch", Why: "ServerReconciler For(&Server{})"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "create", Why: "ServerGroupReconciler erzeugt Server bis zur Untergrenze"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "delete", Why: "Verkleinern, Kappen aufbewahrter Fehlschläge, Verwaisten-Abgleich"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "update", Why: "Finalizer setzen und entfernen"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "status", Verb: "update", Why: "ServerReconciler schreibt Phase, Zeitstempel und Conditions"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion auf den OwnerReferences der Pods in podspec.BuildServerPod"},

	// No entry for proxygroups:get — nichts holt eine einzelne ProxyGroup, der
	// Controller zählt sie nur über eine List.
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "list", Why: "NetworkReconciler zählt Proxy-Gruppen"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "watch", Why: "Cache des Managers"},
}
