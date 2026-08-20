package controller

// Event actions.
//
// The events API (k8s.io/client-go/tools/events, which replaced the deprecated
// tools/record) takes an `action` alongside `reason`, and events.k8s.io/v1
// validation rejects an event that leaves it empty. The two fields answer
// different questions, and Kubernetes' convention is that `reason` says why an
// event happened while `action` says what the controller was doing when it did.
//
// This repository's reasons are already machine-readable and outcome-shaped --
// ProxyPodBlocked, ForwardingSecretRotated, ReadinessDiverged. Restating one of
// those as the action would add a second copy of the same fact and no new
// information. So `action` here is derived from the operation instead, by one
// rule applied at every call site:
//
//  1. If the event reports on the controller's attempt to mutate a subordinate
//     object -- create it, delete it, adopt it, ask it to retire -- the action
//     is <Verb><Kind> naming that attempt. Success and failure of the same
//     attempt share one action and are told apart by `reason`, which is the
//     whole point of having both fields. The r.Create three lines above the
//     event is what names it.
//  2. Otherwise the controller mutated nothing and was computing and writing
//     the reconciled object's own status. The action is actionSyncStatus. Every
//     condition flip and every phase transition below is this case.
//
// Adding an event means choosing from this list, not inventing a new string. If
// nothing here fits, the operation is new and belongs here first.
//
// Note what rule 1 buys, at the two sites that both report reason
// ProxyPodBlocked: ProxyGroupReconciler.Reconcile fires it when the API server
// refuses a pod it tried to create (actionCreateProxyPod), and
// reportBlockedProxies fires it when an already-created pod cannot be scheduled
// (actionSyncStatus). Same reason, different operation -- and now the event
// says which.
const (
	// actionAdoptPod is ServerReconciler deciding whether a pod that already
	// holds the name it wants is one it may take charge of.
	actionAdoptPod = "AdoptPod"
	// actionCreatePod is ServerReconciler creating a game server's pod, from
	// the namespace bootstrap that gates it through to the create itself.
	actionCreatePod = "CreatePod"
	// actionDeletePod is ServerReconciler deleting a game server's pod.
	actionDeletePod = "DeletePod"

	// actionCreateProxyPod is ProxyGroupReconciler creating a proxy pod.
	actionCreateProxyPod = "CreateProxyPod"
	// actionDrainProxy is ProxyGroupReconciler taking a proxy pod out of
	// service, whether because its node is going away or because the drain ran
	// out of time.
	actionDrainProxy = "DrainProxy"

	// actionCreateServer is ServerGroupReconciler creating a member Server,
	// ephemeral or persistent.
	actionCreateServer = "CreateServer"
	// actionDeleteServer is ServerGroupReconciler deleting a member Server.
	actionDeleteServer = "DeleteServer"
	// actionRetireServer is ServerGroupReconciler asking a member Server into
	// soft drain for a rolling update.
	actionRetireServer = "RetireServer"

	// actionSyncStatus is rule 2: the controller changed nothing but its own
	// object's status, and the event reports what it observed while doing so.
	actionSyncStatus = "SyncStatus"
)

// A note on the `related` argument, which is nil at every call site in this
// package.
//
// The events API's `related` names a secondary object for events about two of
// them at once. Every event this operator emits regards exactly one object, and
// the tools/record call sites this package migrated from had no way to carry a
// second one. Passing the created Server or the deleted Pod there would be new
// information in the operator's output that no test covers and no design asked
// for, so the migration leaves it nil throughout and says why once, here,
// rather than at twenty-three call sites.
