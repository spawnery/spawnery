//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// theNetworkGetsItsPolicy asserts the object, and only the object: nothing
// here listens, and kindnet enforces no NetworkPolicy.
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
		// The UID too: a delete-and-recreate leaves the name but not the UID.
		owner := policy.OwnerReferences[0]
		var network spawneryv1alpha1.Network
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "production"}, &network); err != nil {
			return false, "reading the Network to compare its UID: " + err.Error()
		}
		switch {
		case owner.Kind != "Network":
			return false, fmt.Sprintf("owned by a %s", owner.Kind)
		case owner.Name != "production":
			return false, fmt.Sprintf("owned by %q", owner.Name)
		case owner.UID != network.UID:
			return false, fmt.Sprintf("owned by UID %s, but the Network is %s", owner.UID, network.UID)
		}
		return true, ""
	})
}

// theOperatorStaysReadyBehindItsOwnPolicy: the operator's NetworkPolicy must
// still admit the kubelet's probe. kindnet enforces nothing, so this can only
// fail once the harness has an enforcing CNI.
func theOperatorStaysReadyBehindItsOwnPolicy(t *testing.T) {
	var policy networkingv1.NetworkPolicy
	key := client.ObjectKey{Namespace: operatorNamespace, Name: "spawnery-operator-agent"}
	if err := k8s.Get(ctx, key, &policy); err != nil {
		t.Fatalf("the operator's own policy was never applied: %v", err)
	}

	// Held: a probe failure takes three periods to move the pod out of Ready.
	eventuallyStable(t, time.Minute, 20*time.Second,
		"the operator ready behind its own policy", func() (bool, string) {
			pod := operatorPod(t, operatorNamespace)
			for _, c := range pod.Status.ContainerStatuses {
				if !c.Ready {
					return false, fmt.Sprintf("container not ready, restarts %d", c.RestartCount)
				}
			}
			return true, ""
		})
}
