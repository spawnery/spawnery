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
// kind runs no load balancer controller, so this test writes the ingress
// entry itself. The address appearing once a proxy is ready is covered by
// TestTheLoadBalancerAddressAppearsOnceAProxyIsReady in envtest.
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

// The ClusterIP group gets a Service with no way out of the cluster of its
// own: cluster-internal type, no node port, no external traffic policy.
//
// The published address is not asserted: no pod here becomes ready. See
// TestProxyAddressPublishesOnlyWhatIsObservablyRealised.
func theClusterIPGroupGetsAPlainServiceWithNoNodePort(t *testing.T) {
	var svc corev1.Service
	eventually(t, 2*time.Minute, "the gateway-clusterip Service", func() (bool, string) {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-clusterip"}, &svc); err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeClusterIP {
			return false, "type is " + string(svc.Spec.Type)
		}
		return true, ""
	})

	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %s, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("got %d ports, want one", len(svc.Spec.Ports))
	}
	if got := svc.Spec.Ports[0].NodePort; got != 0 {
		t.Errorf("nodePort = %d, want 0", got)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want empty", svc.Spec.ExternalTrafficPolicy)
	}
}

// theHostPortGroupBindsThePortAndHasNoService is the strategy with no Service
// at all: nothing inside the cluster dials a proxy, so there is nothing for
// one to do.
//
// Two replicas on a one-node cluster: the unschedulable one must be explained
// on the group.
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
// exercised under the operator's own ServiceAccount.
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

// aForbiddenHostPortIsReportedOnTheGroup: Pod Security baseline refuses the
// host port. This puts an `is forbidden:` line in the operator's log on
// purpose, which denialsIn excludes.
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
		// Both substrings: an unrelated baseline violation must not pass.
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
