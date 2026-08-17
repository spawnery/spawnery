//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// aPersistentGroupsClaimOutlivesItsServer is milestone 5a's central property,
// checked in a real cluster for the first time by anything but a person.
//
// Two halves, and neither is the one thing envtest cannot do -- that
// distinction is narrower than it first looks, and worth being exact about
// after fix round 1 found the first draft of this comment overstating it.
//
// The first half reads the owner-reference field directly off each claim.
// envtest can do this too, two ways that need no garbage collector at all:
// podspec.TestBuildDataClaim is a plain unit test with no cluster whatsoever,
// and TestAPersistentServerGetsItsClaimBeforeItsPod reads the same field off
// the object envtest's API server actually persisted. Both catch an owner
// reference the moment BuildDataClaim sets one -- confirmed here by mutation,
// not assumed: giving the built claim an owner reference turned both of those
// tests, and `make test`, red before this package was ever involved. So a
// regression that adds one is already caught three ways before a real
// cluster runs at all, and this half of the scenario is corroboration, not a
// capability unique to it.
//
// The second half is where a real garbage collector is actually the point.
// TestDeletingAPersistentServerLeavesItsClaim's own doc comment states the
// limitation precisely: it cannot catch an owner reference either, because
// envtest runs no garbage collector, so an owned claim there would survive
// its deleted owner exactly as an unowned one does -- indistinguishable by
// that test's own assertion. That gap is not about whether the field is set
// (the first half already covers that); it is about whether, in a system
// that actually enforces cascading deletion, an owner reference's *presence*
// would actually destroy the claim. Only a real cluster can show that
// consequence, and only by giving its garbage collector a real, bounded
// window to act -- not a single synchronous read taken the instant the
// Server object disappears, which fix round 1 also found: real garbage
// collection is asynchronous with the owner's removal from etcd, and a bare
// point-in-time check after survival-1 goes NotFound can run before the
// collector has processed the deletion event at all. See the eventuallyStable
// call below, and task-7-report.md's fix-round-1 section for the run that
// caught this and the run that confirms the fix.
func aPersistentGroupsClaimOutlivesItsServer(t *testing.T) {
	eventually(t, 2*time.Minute, "both ordinals' claims", func() (bool, string) {
		claims := claimsIn(t)
		return len(claims) == 2, fmt.Sprintf("%d claims: %v", len(claims), claimNames(claims))
	})

	for _, c := range claimsIn(t) {
		if len(c.OwnerReferences) != 0 {
			t.Errorf("claim %s carries %d owner reference(s): %v. A world with an owner "+
				"is deleted with it -- by the garbage collector, silently, and only in a "+
				"real cluster",
				c.Name, len(c.OwnerReferences), c.OwnerReferences)
		}
	}

	// The top ordinal goes; its claim stays. This is the route milestone 5a's
	// own evidence run never drove -- it deleted a pod by hand -- and 5b's did.
	var g spawneryv1alpha1.ServerGroup
	key := client.ObjectKey{Namespace: testNamespace, Name: "survival"}
	if err := k8s.Get(ctx, key, &g); err != nil {
		t.Fatalf("get ServerGroup survival: %v", err)
	}
	before := claimNames(claimsIn(t))
	patch := client.MergeFrom(g.DeepCopy())
	one := int32(1)
	g.Spec.Replicas = &one
	if err := k8s.Patch(ctx, &g, patch); err != nil {
		t.Fatalf("patch survival to replicas=1: %v", err)
	}

	eventually(t, 3*time.Minute, "survival-1 to go", func() (bool, string) {
		var s spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "survival-1"}, &s)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "still there, phase " + s.Status.Phase
	})

	// eventuallyStable, not a single read: the Server going NotFound only
	// says the API server has forgotten the owner, not that kube-controller-
	// manager's garbage collector has finished reacting to it -- that
	// reaction is asynchronous and unions with nothing this test has already
	// waited on. Holding the count for a window gives a real garbage
	// collector time to prove the negative (nothing to reap, because nothing
	// here is owned) or the positive (something was reaped, under mutation).
	// A single immediate read cannot tell "GC ran and found nothing" apart
	// from "GC hasn't run yet"; this can.
	eventuallyStable(t, time.Minute, 15*time.Second,
		"survival-1's claim to still be there, unreaped by a collector it should never attract",
		func() (bool, string) {
			after := claimNames(claimsIn(t))
			return len(after) == len(before), fmt.Sprintf("%d claim(s): %v, want %v", len(after), after, before)
		})
}

// theProxyGroupGetsItsService checks the one object a ProxyGroup owns that
// nothing else in this run produces.
func theProxyGroupGetsItsService(t *testing.T) {
	eventually(t, 2*time.Minute, "the gateway Service", func() (bool, string) {
		var svc corev1.Service
		err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway"}, &svc)
		if err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeNodePort {
			return false, "type is " + string(svc.Spec.Type)
		}
		for _, p := range svc.Spec.Ports {
			if p.NodePort == 30765 {
				return true, ""
			}
		}
		return false, fmt.Sprintf("ports %v", svc.Spec.Ports)
	})
}

// theOperatorHoldsItsSecretAndItsLease checks the two things startup alone
// drives, and the two that run through the markers carrying
// namespace=spawnery-system as a literal. If that qualifier is ever wrong, the
// operator fails at certs.Ensure or during leader election -- and RBAC never
// says where the problem is. See docs/known-issues.md, "spawnery-system is
// hard-wired into the RBAC markers".
func theOperatorHoldsItsSecretAndItsLease(t *testing.T) {
	var secrets corev1.SecretList
	if err := k8s.List(ctx, &secrets, client.InNamespace(operatorNamespace)); err != nil {
		t.Fatalf("list secrets in %s: %v", operatorNamespace, err)
	}
	found := false
	for _, s := range secrets.Items {
		if s.Type == corev1.SecretTypeTLS {
			found = true
		}
	}
	if !found {
		t.Errorf("no TLS secret in %s: certs.Store.Ensure never wrote its serving "+
			"certificate, and every agent would fail its handshake", operatorNamespace)
	}

	var leases coordinationv1.LeaseList
	if err := k8s.List(ctx, &leases, client.InNamespace(operatorNamespace)); err != nil {
		t.Fatalf("list leases in %s: %v", operatorNamespace, err)
	}
	if len(leases.Items) == 0 {
		t.Errorf("no Lease in %s, yet the Deployment passes --leader-elect=true and the "+
			"readiness probe only turns green once the lock is held", operatorNamespace)
	}
}

func claimsIn(t *testing.T) []corev1.PersistentVolumeClaim {
	t.Helper()
	var list corev1.PersistentVolumeClaimList
	if err := k8s.List(ctx, &list, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list claims: %v", err)
	}
	return list.Items
}

func claimNames(claims []corev1.PersistentVolumeClaim) []string {
	names := make([]string, 0, len(claims))
	for _, c := range claims {
		names = append(names, c.Name)
	}
	return names
}
