//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/podspec"
)

// theNetworkGetsItsPolicy asserts the object, and only the object.
//
// It cannot assert that anything is blocked, and saying so here rather than in
// a document is deliberate. Two reasons compound. No image in this harness
// resolves, by decision, so no process listens on 25565 and there is nothing to
// connect from or to. And enforcement is a property of the CNI rather than of
// the object: hack/e2e.sh runs a bare `kind create cluster` with the default
// kindnet, and if that CNI drops nothing then "the connection was blocked" and
// "the policy was never applied" produce the same green. The enforcement claim
// belongs to the RKE2 rollout at the end of milestone 6.
func theNetworkGetsItsPolicy(t *testing.T) {
	eventually(t, 2*time.Minute, "the production network's policy", func() (bool, string) {
		var policy networkingv1.NetworkPolicy
		key := client.ObjectKey{
			Namespace: testNamespace,
			Name:      podspec.NetworkPolicyName("production"),
		}
		if err := k8s.Get(ctx, key, &policy); err != nil {
			return false, err.Error()
		}
		if got := policy.Spec.PodSelector.MatchLabels[podspec.LabelRole]; got != podspec.RoleServer {
			return false, fmt.Sprintf("selects role %q", got)
		}
		if len(policy.OwnerReferences) != 1 {
			return false, fmt.Sprintf("%d owner references", len(policy.OwnerReferences))
		}
		return true, ""
	})
}

// theOperatorStaysReadyBehindItsOwnPolicy is acceptance criterion 6, and it is
// the one place milestone 6b touches probe traffic at all.
//
// config/deploy/networkpolicy.yaml selects the operator pod, which makes it
// default-deny for ingress -- including the kubelet's probe on the health
// port. If the peerless rule admitting it were wrong, the pod would go
// NotReady and the Deployment would stop being Available.
//
// What this cannot claim: Task 3 of this milestone measured, rather than
// assumed, that kindnet -- the CNI hack/e2e.sh's bare `kind create cluster`
// gets by default -- does not enforce NetworkPolicy ingress at all. Its
// evidence: with the peerless probe rule deleted from
// config/deploy/networkpolicy.yaml, leaving a policy that admits only the
// agent peer on 9443 and denies the kubelet's probe outright, `make e2e`
// stayed green and the rollout succeeded on its usual timeline (see
// task-3-report.md, "Mutation 2"). Two alternatives were ruled out there: the
// readiness probe is a real httpGet and `kubectl rollout status` cannot
// succeed without one passing, so the probe path was genuinely exercised; and
// hack/e2e.sh recreates the cluster every run, with the apply log showing the
// policy `created` rather than `unchanged`, so the policy was genuinely in
// force. That leaves only one explanation: kindnet let the denied traffic
// through anyway.
//
// So on this harness the operator stays Ready behind a correct policy and
// would stay Ready behind a wrong one too -- nothing this scenario observes
// tells the two apart, and it cannot fail for the reason its name suggests.
// It is kept anyway, as a regression guard for the day the harness gains an
// enforcing CNI: on that day, and only on that day, a wrong peerless rule
// would turn this red. The enforcement claim itself belongs to the RKE2
// rollout at the end of milestone 6, same as theNetworkGetsItsPolicy above.
func theOperatorStaysReadyBehindItsOwnPolicy(t *testing.T) {
	var policy networkingv1.NetworkPolicy
	key := client.ObjectKey{Namespace: operatorNamespace, Name: "spawnery-operator-agent"}
	if err := k8s.Get(ctx, key, &policy); err != nil {
		t.Fatalf("the operator's own policy was never applied: %v", err)
	}

	// Held rather than sampled: a probe failure takes three periods to move
	// the pod out of Ready, and hack/e2e.sh's rollout wait returned before
	// that could have happened.
	eventuallyStable(t, time.Minute, 20*time.Second,
		"the operator ready behind its own policy", func() (bool, string) {
			pod := operatorPod(t)
			for _, c := range pod.Status.ContainerStatuses {
				if !c.Ready {
					return false, fmt.Sprintf("container not ready, restarts %d", c.RestartCount)
				}
			}
			return true, ""
		})
}
