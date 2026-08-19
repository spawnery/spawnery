//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// theLoadBalancerGroupGetsItsService checks the Service, and then checks that
// an assigned address is NOT published while nothing is serving.
//
// The name says Service rather than address on purpose. kind runs no load
// balancer controller, so the ingress entry below is written by this test.
// What that proves is the readiness gate on proxyAddress: no image in this
// run's manifest resolves, so no proxy is ever ready, and status.address must
// stay empty even with an address assigned. The other half of that branch --
// the address appearing once both conditions hold -- is proven in envtest, by
// TestTheLoadBalancerAddressAppearsOnceAProxyIsReady
// (`internal/controller/expose_test.go`), which drives a live Reconcile with
// an assigned ingress and a pod it marks Ready itself and watches
// status.address come out non-empty. Nothing here says a load balancer was
// involved, because none was, and nothing there does either.
func theLoadBalancerGroupGetsItsService(t *testing.T) {
	var svc corev1.Service
	eventually(t, 2*time.Minute, "the gateway-lb Service", func() (bool, string) {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-lb"}, &svc); err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			return false, "type is " + string(svc.Spec.Type)
		}
		return true, ""
	})

	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local. Cluster SNATs the client "+
			"address away, and bans and rate limits are built on it",
			svc.Spec.ExternalTrafficPolicy)
	}
	if svc.Annotations["metallb.universe.tf/address-pool"] != "spawnery-e2e" {
		t.Errorf("annotations = %+v, want the manifest's address-pool. A pool selector "+
			"that does not reach the Service is how a LoadBalancer group silently "+
			"lands in the wrong network", svc.Annotations)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 25565 {
		t.Errorf("ports = %+v, want exactly 25565", svc.Spec.Ports)
	}

	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := k8s.Status().Update(ctx, &svc); err != nil {
		t.Fatalf("write an ingress address the way a load balancer controller would: %v", err)
	}

	eventuallyStable(t, 90*time.Second, 30*time.Second,
		"status.address to stay empty while no proxy is ready", func() (bool, string) {
			var group spawneryv1alpha1.ProxyGroup
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-lb"}, &group); err != nil {
				return false, err.Error()
			}
			if group.Status.Address != "" {
				return false, "address is " + group.Status.Address
			}
			return true, ""
		})
}

// theHostPortGroupBindsThePortAndHasNoService is the strategy with no Service
// at all: nothing inside the cluster dials a proxy, so there is nothing for
// one to do.
//
// The group asks for two replicas on a one-node cluster. hostPort lets the
// scheduler place one of them, and the other is the surplus -- which the
// group has to explain, because a pod that exists, never runs, and never will
// is otherwise indistinguishable from one that is merely slow.
func theHostPortGroupBindsThePortAndHasNoService(t *testing.T) {
	eventually(t, 2*time.Minute, "a gateway-host pod carrying the host port", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods, client.InNamespace(testNamespace),
			client.MatchingLabels{podspec.LabelGroup: "gateway-host"}); err != nil {
			return false, err.Error()
		}
		if len(pods.Items) == 0 {
			return false, "no pods yet"
		}
		for _, p := range pods.Items {
			for _, port := range p.Spec.Containers[0].Ports {
				if port.Name == "minecraft" && port.HostPort == 25565 {
					return true, ""
				}
			}
		}
		return false, fmt.Sprintf("%d pod(s), none binding 25565", len(pods.Items))
	})

	var svc corev1.Service
	err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-host"}, &svc)
	if !apierrors.IsNotFound(err) {
		t.Errorf("a HostPort group has a Service (err = %v). Nothing in the cluster "+
			"dials a proxy, so the object has no consumer and its node port is "+
			"held for nobody", err)
	}

	eventually(t, 3*time.Minute, "the group to report the pod it cannot place", func() (bool, string) {
		var group spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-host"}, &group); err != nil {
			return false, err.Error()
		}
		cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
		if cond == nil {
			return false, "no Degraded condition"
		}
		if cond.Status != metav1.ConditionTrue {
			return false, "Degraded is " + string(cond.Status) + "/" + cond.Reason
		}
		if cond.Reason != spawneryv1alpha1.ReasonProxyPodUnschedulable {
			return false, "reason is " + cond.Reason
		}
		return true, "message: " + cond.Message
	})
}

// aSwitchToHostPortRemovesTheService is the only place services: delete is
// exercised under the operator's own ServiceAccount. Everything else about
// that permission is a table entry and a generated role.
func aSwitchToHostPortRemovesTheService(t *testing.T) {
	eventually(t, 2*time.Minute, "the gateway-switch Service", func() (bool, string) {
		var svc corev1.Service
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-switch"}, &svc); err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeNodePort {
			return false, "type is " + string(svc.Spec.Type)
		}
		return true, ""
	})

	var group spawneryv1alpha1.ProxyGroup
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-switch"}, &group); err != nil {
		t.Fatalf("get gateway-switch: %v", err)
	}
	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25566},
	}
	if err := k8s.Update(ctx, &group); err != nil {
		t.Fatalf("switch gateway-switch to HostPort: %v", err)
	}

	eventually(t, 2*time.Minute, "the Service to be removed", func() (bool, string) {
		var svc corev1.Service
		err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-switch"}, &svc)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "the Service is still there, holding node port " +
			fmt.Sprint(svc.Spec.Ports[0].NodePort)
	})

	eventually(t, 3*time.Minute, "a pod carrying the new host port", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods, client.InNamespace(testNamespace),
			client.MatchingLabels{podspec.LabelGroup: "gateway-switch"}); err != nil {
			return false, err.Error()
		}
		for _, p := range pods.Items {
			for _, port := range p.Spec.Containers[0].Ports {
				if port.Name == "minecraft" && port.HostPort == 25566 {
					return true, ""
				}
			}
		}
		return false, fmt.Sprintf("%d pod(s), none binding 25566", len(pods.Items))
	})
}

// aForbiddenHostPortIsReportedOnTheGroup is the one refusal this repository
// has ever observed being enforced.
//
// Everything milestone 6b shipped is an object whose effect no run here can
// see: kindnet enforces no NetworkPolicy, so a correct policy and a wholly
// broken one produce the same green. This is different in kind. Pod Security
// baseline disallows host ports, the API server enforces it, and the pod
// genuinely does not come into existence. It is also the reason
// theOperatorWasNeverDenied excludes `violates PodSecurity`: this scenario
// puts an `is forbidden:` line in the operator's log on purpose.
func aForbiddenHostPortIsReportedOnTheGroup(t *testing.T) {
	const ns = "minecraft-baseline"

	eventually(t, 3*time.Minute, "the group to carry the API server's refusal", func() (bool, string) {
		var group spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gateway-forbidden"}, &group); err != nil {
			return false, err.Error()
		}
		cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
		if cond == nil {
			return false, "no Degraded condition"
		}
		if cond.Status != metav1.ConditionTrue || cond.Reason != spawneryv1alpha1.ReasonProxyPodRejected {
			return false, "Degraded is " + string(cond.Status) + "/" + cond.Reason
		}
		// Both substrings, not just the first. If the proxy pod acquired some
		// unrelated baseline violation and the container host port were
		// dropped entirely, a PodSecurity-only assertion would stay green
		// with the strategy under test gone.
		for _, want := range []string{"PodSecurity", "hostPort"} {
			if !strings.Contains(cond.Message, want) {
				return false, "message does not name " + want + ": " + cond.Message
			}
		}
		return true, ""
	})

	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(ns),
		client.MatchingLabels{podspec.LabelGroup: "gateway-forbidden"}); err != nil {
		t.Fatalf("list the group's pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("%d pod(s) exist in a namespace that forbids host ports. Either the "+
			"label is not on the namespace or the pod does not carry the port, and "+
			"in both cases this scenario has been measuring nothing", len(pods.Items))
	}
}
