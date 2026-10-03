//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// aPersistentGroupsClaimOutlivesItsServer checks that a claim carries no owner
// reference, and then, which envtest cannot, that a real garbage collector
// leaves it standing after its Server is deleted.
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

	// The top ordinal goes; its claim stays.
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

	// Held for a window: garbage collection is asynchronous to the owner's
	// removal.
	eventuallyStable(t, time.Minute, 15*time.Second,
		"survival-1's claim to still be there, unreaped by a collector it should never attract",
		func() (bool, string) {
			after := claimNames(claimsIn(t))
			return len(after) == len(before), fmt.Sprintf("%d claim(s): %v, want %v", len(after), after, before)
		})
}

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

// theOperatorHoldsItsSecretAndItsLease checks the two objects whose RBAC
// markers carry namespace=spawnery-system as a literal.
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
