/*
Copyright paul_wtf.

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

package controller

import (
	"net"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// No node port named: the API server allocates one anyway, and naming one invites
// collisions between groups in different namespaces.
func TestLoadBalancerServiceShape(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
			},
		}
	})

	svc, err := r.reconcileService(f.ctx, group)
	if err != nil {
		t.Fatalf("reconcileService: %v", err)
	}
	if svc == nil {
		t.Fatal("reconcileService returned no Service for a LoadBalancer group")
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Service type = %q, want LoadBalancer", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Errorf("externalTrafficPolicy = %q, want the Cluster the spec asked for",
			svc.Spec.ExternalTrafficPolicy)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("ports = %+v, want exactly the Minecraft port", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Port != podspec.MinecraftPort {
		t.Errorf("port = %d, want %d", svc.Spec.Ports[0].Port, podspec.MinecraftPort)
	}
	if svc.Spec.Selector[podspec.LabelRole] != podspec.RoleProxy {
		t.Error("the selector must pin the proxy role, or it would also select server pods")
	}
}

// A unit-built ProxyGroup skips API-server defaulting, so the Local default must exist
// in code as well as in the marker.
func TestLoadBalancerDefaultsToLocalWithoutTheAPIServer(t *testing.T) {
	group := &spawneryv1alpha1.ProxyGroup{
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
		},
	}
	if got := loadBalancerTrafficPolicy(group); got != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local", got)
	}

	group.Spec.Expose.LoadBalancer = nil
	if got := loadBalancerTrafficPolicy(group); got != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("with no loadBalancer block at all, externalTrafficPolicy = %q, want Local", got)
	}
}

// A Service left behind would keep the group reachable by the route the switch ended.
func TestSwitchingToHostPortDeletesTheService(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway")

	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService as NodePort: %v", err)
	}
	var before corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &before); err != nil {
		t.Fatalf("the NodePort Service was not created: %v", err)
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	}
	svc, err := r.reconcileService(f.ctx, group)
	if err != nil {
		t.Fatalf("reconcileService as HostPort: %v", err)
	}
	if svc != nil {
		t.Errorf("a HostPort group got a Service: %+v", svc.Spec)
	}

	var after corev1.Service
	err = f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &after)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the Service survived the switch to HostPort (err = %v); it still holds "+
			"node port %d and still selects this group's pods",
			err, before.Spec.Ports[0].NodePort)
	}
}

func TestSwitchingToHostPortLeavesAForeignServiceAlone(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})

	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: f.ns},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Ports:    []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
			Selector: map[string]string{"app": "somebody-elses"},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create the foreign Service: %v", err)
	}

	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}

	var after corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &after); err != nil {
		t.Fatalf("the operator deleted a Service it does not own: %v", err)
	}
}

// Load-balancer controllers annotate the same Service, so the operator records the keys
// it set and removes only those.
func TestLoadBalancerAnnotationsAreOwnedAndReleased(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				Annotations: map[string]string{
					"metallb.universe.tf/address-pool":    "minecraft",
					"metallb.universe.tf/allow-shared-ip": "spawnery",
				},
			},
		}
	})

	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}

	// A third party annotates the Service as a load-balancer controller would.
	var svc corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if svc.Annotations["metallb.universe.tf/address-pool"] != "minecraft" {
		t.Fatalf("the spec's annotation did not reach the Service: %+v", svc.Annotations)
	}
	svc.Annotations["metallb.universe.tf/ip-allocated-from-pool"] = "minecraft"
	if err := f.c.Update(f.ctx, &svc); err != nil {
		t.Fatalf("annotate the Service as a third party would: %v", err)
	}

	// The user drops one of the two keys they had set.
	group.Spec.Expose.LoadBalancer.Annotations = map[string]string{
		"metallb.universe.tf/address-pool": "minecraft",
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService after the spec changed: %v", err)
	}

	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if _, still := svc.Annotations["metallb.universe.tf/allow-shared-ip"]; still {
		t.Error("an annotation removed from the spec survived on the Service; a user " +
			"who removes one sees nothing happen, permanently")
	}
	if svc.Annotations["metallb.universe.tf/address-pool"] != "minecraft" {
		t.Error("the annotation still in the spec was removed too")
	}
	if svc.Annotations["metallb.universe.tf/ip-allocated-from-pool"] != "minecraft" {
		t.Error("the operator removed an annotation it never set. That key belongs to " +
			"the load balancer controller, and taking it away is how a working " +
			"allocation gets torn down")
	}
}

// The bookkeeping key goes too, or the next LoadBalancer group at that name inherits it.
func TestLeavingLoadBalancerReleasesTheAnnotations(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				Annotations: map[string]string{"metallb.universe.tf/address-pool": "minecraft"},
			},
		}
	})
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService as NodePort: %v", err)
	}

	var svc corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if _, still := svc.Annotations["metallb.universe.tf/address-pool"]; still {
		t.Error("a LoadBalancer annotation survived the switch to NodePort")
	}
	if _, still := svc.Annotations[podspec.AnnotationExposeAnnotations]; still {
		t.Error("the bookkeeping key survived with nothing left to account for")
	}
}

// The enum makes the false branch unreachable for a real object. The guard puts an
// unhandled future value on the group where a user reads it; only a pure function lets
// a test reach it.
func TestExposeImplementedCoversTheEnumAndNothingElse(t *testing.T) {
	for _, known := range []spawneryv1alpha1.ExposeType{
		spawneryv1alpha1.ExposeNodePort,
		spawneryv1alpha1.ExposeLoadBalancer,
		spawneryv1alpha1.ExposeHostPort,
		spawneryv1alpha1.ExposeClusterIP,
	} {
		if !exposeImplemented(known) {
			t.Errorf("%s is in the CRD's enum, so a user can create a group asking for "+
				"it, and this operator refuses it", known)
		}
	}
	for _, unknown := range []spawneryv1alpha1.ExposeType{"", "Anycast", "nodeport"} {
		if exposeImplemented(unknown) {
			t.Errorf("%q is accepted as implemented; reconcileService has no branch "+
				"for it and would fail the reconcile with an error only the log "+
				"sees, instead of refusing the group where a user would find out",
				unknown)
		}
	}
}

// No pod is made ready here, so status.address is covered by
// TestTheClusterIPAddressAppearsOnceAProxyIsReady instead.
func TestReconcileAcceptsEveryStrategy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		expose       spawneryv1alpha1.ExposeSpec
		wantSvc      bool
		wantType     corev1.ServiceType
		wantHostPort int32
	}{
		{
			name: "NodePort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			wantSvc: true, wantType: corev1.ServiceTypeNodePort,
		},
		{
			name: "LoadBalancer",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			wantSvc: true, wantType: corev1.ServiceTypeLoadBalancer,
		},
		{
			name: "ClusterIP",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
			},
			wantSvc: true, wantType: corev1.ServiceTypeClusterIP,
		},
		{
			name: "HostPort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			wantSvc: false, wantHostPort: 25565,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := proxyGroupReconciler(f)
			f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
				g.Spec.Expose = tc.expose
			})

			f.reconcileProxyGroup(r, "gateway")

			group := f.proxyGroup("gateway")
			if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
				metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
				t.Fatalf("conditions = %+v, want Accepted=True", group.Status.Conditions)
			}

			pods := f.proxyPods("gateway")
			if len(pods) == 0 {
				t.Fatal("an accepted group created no proxy pods")
			}
			var hostPort int32
			for _, p := range pods[0].Spec.Containers[0].Ports {
				if p.Name == podspec.MinecraftPortName {
					hostPort = p.HostPort
				}
			}
			if hostPort != tc.wantHostPort {
				t.Errorf("the pod's minecraft hostPort = %d, want %d", hostPort, tc.wantHostPort)
			}

			var svc corev1.Service
			err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc)
			switch {
			case tc.wantSvc && err != nil:
				t.Fatalf("no Service for a %s group: %v", tc.name, err)
			case tc.wantSvc && svc.Spec.Type != tc.wantType:
				t.Errorf("Service type = %q, want %q", svc.Spec.Type, tc.wantType)
			case !tc.wantSvc && !apierrors.IsNotFound(err):
				t.Errorf("a HostPort group got a Service (err = %v)", err)
			}
		})
	}
}

// envtest runs PodSecurity admission, so the label below is enforced as in a cluster.
func TestARejectedProxyPodIsReportedOnTheGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.enforcePodSecurity(t, "baseline")
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})

	// The reconcile returns the API server's error, so reconcileProxyGroup is the wrong helper.
	_, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	})
	if err == nil {
		t.Fatal("the reconcile succeeded in a namespace that forbids host ports")
	}

	group := f.proxyGroup("gateway")
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True. Without it the group reports Pending and "+
			"only the operator's log says why", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodRejected {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonProxyPodRejected)
	}
	// Both substrings: PodSecurity alone would stay green if the host port were dropped.
	for _, want := range []string{"PodSecurity", "hostPort"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message = %q, want it to name %q; it must carry the API server's "+
				"own words, because the remedy is in them and nothing else knows it",
				cond.Message, want)
		}
	}
	if group.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", group.Status.Phase)
	}
}

// The refused switch deletes the Service, so its node-port address must not stay published.
func TestAGroupSwitchedIntoARefusedStrategyStopsAdvertisingTheOldAddress(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeNodePort,
			NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765},
		}
	})

	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	if len(pods) != 1 {
		t.Fatalf("proxy pods = %d, want 1", len(pods))
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyGroup("gateway").Status.Address
	if before == "" {
		t.Fatal("the group published no address before the switch, so this test " +
			"cannot show one being withdrawn")
	}

	f.enforcePodSecurity(t, "baseline")
	group := f.proxyGroup("gateway")
	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	}
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("switch the group to HostPort: %v", err)
	}

	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	}); err == nil {
		t.Fatal("the reconcile succeeded in a namespace that forbids host ports")
	}

	// Premise: an old ready pod remains, so an empty address is not just "no pod is ready".
	stillReady := 0
	for _, p := range f.proxyPods("gateway") {
		if isPodReady(&p) {
			stillReady++
		}
	}
	if stillReady == 0 {
		t.Skip("no ready pod survived the switch, so this run cannot distinguish " +
			"the address guard from the readiness gate; see the plan's note")
	}

	after := f.proxyGroup("gateway")
	if after.Status.Address != "" {
		t.Errorf("status.address = %q, want it empty. It was %q before the switch, "+
			"and the Service that node port belonged to has been deleted -- a player "+
			"dialing it reaches nothing", after.Status.Address, before)
	}
	// An empty address alone would look like a group that has not come up yet.
	cond := meta.FindStatusCondition(after.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True beside the empty address", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodRejected {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonProxyPodRejected)
	}
	if after.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", after.Status.Phase)
	}
}

// The proxies, the Service and its players survive a deleted Network, so the address does too.
func TestABrokenNetworkLeavesAWorkingAddressAlone(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeNodePort,
			NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30766},
		}
	})
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	if len(pods) != 1 {
		t.Fatalf("proxy pods = %d, want 1", len(pods))
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyGroup("gateway").Status.Address
	if before == "" {
		t.Fatal("no address to preserve, so this test cannot show it being preserved")
	}

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete Network: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")

	after := f.proxyGroup("gateway")
	if after.Status.Address != before {
		t.Errorf("status.address = %q, want it left at %q — the pods and the Service "+
			"are untouched by a missing Network, so the address still works",
			after.Status.Address, before)
	}
	cond := meta.FindStatusCondition(after.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %+v, want False — the refusal still has to be legible", cond)
	}
}

// reconcileService's SetControllerReference refuses a Service controlled by something
// else, failing reconcileObserved after the Service and ready pod exist without
// changing the Service.
func TestAFailureInsideReconcileObservedLeavesTheAddressAlone(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeNodePort,
			NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30767},
		}
	})
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	if len(pods) != 1 {
		t.Fatalf("proxy pods = %d, want 1", len(pods))
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyGroup("gateway").Status.Address
	if before == "" {
		t.Fatal("the group published no address before the failure, so this test " +
			"cannot show one surviving it")
	}

	var svc corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	svc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "somebody-elses-controller",
		UID:        types.UID("11111111-1111-1111-1111-111111111111"),
		Controller: ptr.To(true),
	}}
	if err := f.c.Update(f.ctx, &svc); err != nil {
		t.Fatalf("give the Service a foreign controller reference: %v", err)
	}

	// An error is exactly what this pass must return, so not reconcileProxyGroup.
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	}); err == nil {
		t.Fatal("the reconcile succeeded despite the Service already having a foreign controller")
	}

	after := f.proxyGroup("gateway")
	if after.Status.Address != before {
		t.Errorf("status.address = %q, want it left at %q — reconcileService failed "+
			"before reconcileObserved's observation completed, and the group's Service "+
			"and ready pod are untouched", after.Status.Address, before)
	}
}

// hostPort allows one pod of a group per node, so surplus replicas stay Pending.
// envtest runs no scheduler, so the condition is written by hand.
func TestAnUnschedulableProxyPodIsReportedOnTheGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to make unschedulable")
	}
	const schedulerSays = "0/1 nodes are available: 1 node(s) didn't have free ports " +
		"for the requested pod ports."
	pods[0].Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  "Unschedulable",
		Message: schedulerSays,
	}}
	if err := f.c.Status().Update(f.ctx, &pods[0]); err != nil {
		t.Fatalf("mark the pod unschedulable: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodUnschedulable {
		t.Errorf("reason = %q, want %q", cond.Reason,
			spawneryv1alpha1.ReasonProxyPodUnschedulable)
	}
	if !strings.Contains(cond.Message, "free ports") {
		t.Errorf("message = %q, want the scheduler's own text", cond.Message)
	}
	if !strings.Contains(cond.Message, pods[0].Name) {
		t.Errorf("message = %q, want the name of the pod that cannot be placed -- with "+
			"several pods the group's condition is otherwise unattributable",
			cond.Message)
	}
}

// Otherwise the condition would latch True after any transient refusal.
func TestAGroupWithItsPodsIsNotDegraded(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil {
		t.Fatal("no Degraded condition at all; False is a verdict and absent is not")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Degraded = %+v, want False", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodsAdmitted {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonProxyPodsAdmitted)
	}
}

func TestARecoveredProxyGroupFiresAnEventOnlyOnTheFlank(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})
	f.reconcileProxyGroup(r, "gateway")
	// Events from reaching the first steady state are not under test.
	drainEvents(rec)

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to make unschedulable")
	}
	pods[0].Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodScheduled,
		Status: corev1.ConditionFalse,
		Reason: "Unschedulable",
		Message: "0/1 nodes are available: 1 node(s) didn't have free ports " +
			"for the requested pod ports.",
	}}
	if err := f.c.Status().Update(f.ctx, &pods[0]); err != nil {
		t.Fatalf("mark the pod unschedulable: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	blocked := drainEvents(rec)
	if !containsEvent(blocked, "ProxyPodBlocked") {
		t.Fatalf("events = %v, want a ProxyPodBlocked", blocked)
	}
	if cond := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionDegraded); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True before the recovery this test is about", cond)
	}

	// envtest runs no scheduler, so the pod's condition is cleared directly.
	pods[0].Status.Conditions = nil
	if err := f.c.Status().Update(f.ctx, &pods[0]); err != nil {
		t.Fatalf("clear the pod's PodScheduled condition: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	recovered := drainEvents(rec)
	if !containsEvent(recovered, "ProxyPodsAdmitted") {
		t.Fatalf("events = %v, want a ProxyPodsAdmitted on the recovery flank", recovered)
	}
	if !containsEventType(recovered, "Normal") {
		t.Errorf("events = %v, want the recovery recorded as Normal", recovered)
	}
	cond := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != spawneryv1alpha1.ReasonProxyPodsAdmitted {
		t.Fatalf("Degraded = %+v, want False/ProxyPodsAdmitted after the recovery", cond)
	}

	// Steady state: nothing transitions, so nothing fires.
	f.reconcileProxyGroup(r, "gateway")
	if steady := drainEvents(rec); containsEvent(steady, "ProxyPodsAdmitted") {
		t.Errorf("events = %v, want no ProxyPodsAdmitted on a pass where nothing changed", steady)
	}
}

// envtest runs the PodSecurity admission plugin, so this is the real control.
func (f *fixture) enforcePodSecurity(t *testing.T, profile string) {
	t.Helper()
	var ns corev1.Namespace
	if err := f.c.Get(f.ctx, client.ObjectKey{Name: f.ns}, &ns); err != nil {
		t.Fatalf("get namespace %s: %v", f.ns, err)
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["pod-security.kubernetes.io/enforce"] = profile
	if err := f.c.Update(f.ctx, &ns); err != nil {
		t.Fatalf("label namespace %s: %v", f.ns, err)
	}
}

// NewProxyName draws a fresh suffix per attempt, so the refusal text differs every
// pass; storing it re-enqueues the group (For() has no predicate) and it spins. The
// stored message must still be the API server's own words: the remedy is in them.
func TestARefusedProxyPodStopsRewritingTheGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.enforcePodSecurity(t, "baseline")
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})

	const passes = 4
	var versions, messages []string
	for i := 1; i <= passes; i++ {
		// Every pass fails while the label stands; reconcileProxyGroup fails on any error.
		if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
		}); err == nil {
			t.Fatalf("pass %d succeeded in a namespace that forbids host ports", i)
		}
		group := f.proxyGroup("gateway")
		cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != spawneryv1alpha1.ReasonProxyPodRejected {
			t.Fatalf("pass %d: Degraded = %+v, want True/%s", i, cond,
				spawneryv1alpha1.ReasonProxyPodRejected)
		}
		versions = append(versions, group.ResourceVersion)
		messages = append(messages, cond.Message)
	}

	// From the second pass on, nothing about the group changes.
	for i := 2; i < passes; i++ {
		if versions[i] != versions[1] {
			t.Fatalf("resourceVersion by pass = %v, want no change after the second. "+
				"A refused pass that rewrites the object turns the ProxyGroup watch "+
				"into an immediate re-enqueue ahead of the backoff, and the operator "+
				"spins at the API server's expense for as long as the refusal stands",
				versions)
		}
		if messages[i] != messages[1] {
			t.Errorf("message by pass = %v, want the stored refusal to hold still", messages)
		}
	}

	// The stored text is the cluster's, verbatim.
	got := messages[len(messages)-1]
	for _, want := range []string{`pods "`, "is forbidden:", "PodSecurity", "hostPort"} {
		if !strings.Contains(got, want) {
			t.Errorf("message = %q, want it to contain %q -- the remedy is in the API "+
				"server's own words and nothing else knows it", got, want)
		}
	}
}

// sameRefusal decides between rewriting the object and reporting a stale remedy.
func TestSameRefusalSeparatesThePodNameFromTheRemedy(t *testing.T) {
	const first = `pods "gateway-kt84" is forbidden: violates PodSecurity ` +
		`"baseline:latest": hostPort (container "velocity" uses hostPort 25565)`
	const retry = `pods "gateway-9xz2" is forbidden: violates PodSecurity ` +
		`"baseline:latest": hostPort (container "velocity" uses hostPort 25565)`
	const other = `pods "gateway-9xz2" is forbidden: exceeded quota: pods, ` +
		`used: 4, limited: 4`
	const scheduler = "gateway-kt84 cannot be scheduled: 0/1 nodes are available: " +
		"1 node(s) didn't have free ports for the requested pod ports."
	const schedulerElsewhere = "gateway-9xz2 cannot be scheduled: 0/1 nodes are available: " +
		"1 node(s) didn't have free ports for the requested pod ports."

	if !sameRefusal(first, retry) {
		t.Errorf("two attempts at the same refused create read as different refusals; "+
			"that is the hot loop\n%q\n%q", first, retry)
	}
	if sameRefusal(first, other) {
		t.Errorf("a quota refusal reads as the PodSecurity one it replaced, so the "+
			"group would keep reporting a remedy that no longer applies\n%q\n%q",
			first, other)
	}
	if sameRefusal(scheduler, schedulerElsewhere) {
		t.Errorf("two different unschedulable pods read as one; the pod's name is the "+
			"only thing making that condition attributable\n%q\n%q",
			scheduler, schedulerElsewhere)
	}
}

// The manager's Service cache is not narrowed by label, so this guard alone stands
// between a stray object at the group's name and a delete that cannot be undone.
func TestSwitchingToHostPortLeavesAServiceItDoesNotOwnAlone(t *testing.T) {
	t.Run("controlled by a different object", func(t *testing.T) {
		f := newFixture(t)
		r := proxyGroupReconciler(f)
		group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
			g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			}
		})

		// A deleted-and-recreated ProxyGroup: the old Service keeps the old UID until garbage
		// collection catches up, and only the UID tells the two apart.
		predecessor := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gateway",
				Namespace: f.ns,
				Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: spawneryv1alpha1.GroupVersion.String(),
					Kind:       "ProxyGroup",
					Name:       group.Name,
					UID:        types.UID("00000000-0000-0000-0000-0000deadbeef"),
					Controller: ptr.To(true),
				}},
			},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeNodePort,
				Ports:    []corev1.ServicePort{{Port: 25565, Protocol: corev1.ProtocolTCP}},
				Selector: podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name),
			},
		}
		if err := f.c.Create(f.ctx, predecessor); err != nil {
			t.Fatalf("create the predecessor's Service: %v", err)
		}

		if _, err := r.reconcileService(f.ctx, group); err != nil {
			t.Fatalf("reconcileService: %v", err)
		}

		var after corev1.Service
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &after); err != nil {
			t.Fatalf("the operator deleted a Service controlled by a different object "+
				"(err = %v). A same-named group recreated after deletion would take "+
				"its predecessor's Service down with it", err)
		}
	})

	t.Run("ours by owner reference but not by label", func(t *testing.T) {
		f := newFixture(t)
		r := proxyGroupReconciler(f)
		group := f.createProxyGroup("gateway")

		// Built as the operator builds it, then stripped of the operator's label.
		if _, err := r.reconcileService(f.ctx, group); err != nil {
			t.Fatalf("reconcileService as NodePort: %v", err)
		}
		var svc corev1.Service
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
			t.Fatalf("get the Service the operator just built: %v", err)
		}
		delete(svc.Labels, podspec.LabelManagedBy)
		if err := f.c.Update(f.ctx, &svc); err != nil {
			t.Fatalf("strip the managed-by label: %v", err)
		}

		group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
		if _, err := r.reconcileService(f.ctx, group); err != nil {
			t.Fatalf("reconcileService as HostPort: %v", err)
		}

		var after corev1.Service
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &after); err != nil {
			t.Fatalf("the operator deleted a Service that does not carry its own label "+
				"(err = %v); design section 4 requires both halves of the guard", err)
		}
	})
}

// envtest runs no kubelet or load-balancer controller, so the test writes the ingress
// and pod readiness itself; what it proves is the Service reaching setStatus.
func TestTheLoadBalancerAddressAppearsOnceAProxyIsReady(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:         spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
		}
	})
	f.reconcileProxyGroup(r, "gateway")

	// What MetalLB or kube-vip would write once it had picked an address.
	var svc corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("the LoadBalancer group got no Service: %v", err)
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := f.c.Status().Update(f.ctx, &svc); err != nil {
		t.Fatalf("assign an ingress address the way a load balancer controller would: %v", err)
	}

	// An assigned address alone publishes nothing.
	f.reconcileProxyGroup(r, "gateway")
	if addr := f.proxyGroup("gateway").Status.Address; addr != "" {
		t.Fatalf("status.address = %q with an assigned ingress and no ready proxy, want "+
			"empty -- the Service knows nothing about whether anything is serving", addr)
	}

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to make ready")
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	want := net.JoinHostPort(svc.Status.LoadBalancer.Ingress[0].IP,
		strconv.Itoa(int(podspec.MinecraftPort)))
	if group.Status.Address != want {
		t.Errorf("status.address = %q, want %q. Both halves are read back from where "+
			"this test wrote them: the host from the Service's assigned ingress, the "+
			"port from the Service's own port rather than any node port",
			group.Status.Address, want)
	}
	if group.Status.ReadyReplicas != 1 {
		t.Errorf("status.readyReplicas = %d, want 1", group.Status.ReadyReplicas)
	}
}

// Separate from the strategy table: only a ready pod lets proxyAddress return anything.
func TestTheClusterIPAddressAppearsOnceAProxyIsReady(t *testing.T) {
	const want = "mc.example.test"

	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:      spawneryv1alpha1.ExposeClusterIP,
			ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: want},
		}
	})
	f.reconcileProxyGroup(r, "gateway")

	// ClusterIP knows its address before any pod exists, so without the gate it would
	// publish on the first reconcile.
	if addr := f.proxyGroup("gateway").Status.Address; addr != "" {
		t.Fatalf("status.address = %q with no ready proxy, want empty -- a configured "+
			"address is not evidence that anything is serving it", addr)
	}

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to make ready")
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	if got := f.proxyGroup("gateway").Status.Address; got != want {
		t.Errorf("status.address = %q, want %q -- the configured address carried out "+
			"through Reconcile and setStatus, not read from proxyAddress directly",
			got, want)
	}
}

// No node port: ClusterIP exists to avoid one that nobody dials and a firewall must cover.
func TestClusterIPServiceShape(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:      spawneryv1alpha1.ExposeClusterIP,
			ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
		}
	})

	svc, err := r.reconcileService(f.ctx, group)
	if err != nil {
		t.Fatalf("reconcileService: %v", err)
	}
	if svc == nil {
		t.Fatal("no Service was created; a ClusterIP group is fronted by something that needs one to route to")
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %s, want ClusterIP", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want empty: the field is meaningless on a "+
			"ClusterIP Service and the API server rejects it", svc.Spec.ExternalTrafficPolicy)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("got %d ports, want exactly one", len(svc.Spec.Ports))
	}
	if got := svc.Spec.Ports[0].NodePort; got != 0 {
		t.Errorf("nodePort = %d, want 0: the strategy exists so that no node port is "+
			"allocated for a group nobody dials on a node", got)
	}
	if got := svc.Spec.Ports[0].Port; got != podspec.MinecraftPort {
		t.Errorf("port = %d, want %d", got, podspec.MinecraftPort)
	}
	if len(svc.Annotations) != 0 {
		t.Errorf("annotations = %v, want none: nothing reads annotations on a ClusterIP "+
			"Service that could change where traffic goes, and external-dns with "+
			"--publish-internal-services would publish the ClusterIP itself", svc.Annotations)
	}
}

// The API server clears externalTrafficPolicy on the type change; the node port is
// cleared both by reconcileService replacing Ports wholesale and by the API server, so
// that half cannot fail on either alone. Neither arm sets these fields explicitly: no
// test could fail without such a line.
func TestNeitherNodePortNorTrafficPolicySurvivesTheMoveToClusterIP(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway")

	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService as NodePort: %v", err)
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService as ClusterIP: %v", err)
	}

	var stored corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &stored); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if stored.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %s, want ClusterIP", stored.Spec.Type)
	}
	if stored.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want empty: the API server should have "+
			"cleared what the NodePort Service this group used to be left behind, "+
			"since reconcileService's ClusterIP arm never resets the field itself",
			stored.Spec.ExternalTrafficPolicy)
	}
	if got := stored.Spec.Ports[0].NodePort; got != 0 {
		t.Errorf("nodePort = %d, want 0: it was allocated under the previous strategy "+
			"and nothing dials it now. Two mechanisms each force this to zero -- "+
			"reconcileService rebuilding the port from a literal that names no node "+
			"port, and the API server normalising the field away on the type change -- "+
			"so a nonzero result means BOTH have changed, and neither alone is the "+
			"thing to go looking at first", got)
	}
}

func TestSwitchingFromLoadBalancerToClusterIPReleasesTheAnnotations(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				Annotations: map[string]string{"lbipam.cilium.io/ips": "203.0.113.5"},
			},
		}
	})
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var svc corev1.Service
	key := client.ObjectKey{Namespace: f.ns, Name: group.Name}
	if err := f.c.Get(f.ctx, key, &svc); err != nil {
		t.Fatalf("reading the Service back: %v", err)
	}
	svc.Annotations["someone.else/key"] = "left alone"
	if err := f.c.Update(f.ctx, &svc); err != nil {
		t.Fatalf("adding a foreign annotation: %v", err)
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var stored corev1.Service
	if err := f.c.Get(f.ctx, key, &stored); err != nil {
		t.Fatalf("reading the Service back: %v", err)
	}
	if _, still := stored.Annotations["lbipam.cilium.io/ips"]; still {
		t.Error("the operator's own annotation survived the move off LoadBalancer")
	}
	if stored.Annotations["someone.else/key"] != "left alone" {
		t.Error("a foreign annotation was removed; the operator releases only what it set")
	}
}

func TestSwitchingFromHostPortToClusterIPCreatesTheService(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	key := client.ObjectKey{Namespace: f.ns, Name: group.Name}
	var absent corev1.Service
	if err := f.c.Get(f.ctx, key, &absent); err == nil {
		t.Fatal("a HostPort group left a Service behind; the rest of this test proves nothing")
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var stored corev1.Service
	if err := f.c.Get(f.ctx, key, &stored); err != nil {
		t.Fatalf("no Service after moving to ClusterIP: %v", err)
	}
	if stored.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("type = %s, want ClusterIP", stored.Spec.Type)
	}
}

// A roll after an operator upgrade looks like any unrelated fault unless the group says why.
func TestAGroupSaysWhenItsPodsNoLongerMatchWhatTheOperatorRenders(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
	})
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	if len(pods) != 1 {
		t.Fatalf("proxy pods = %d, want 1", len(pods))
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	settled := meta.FindStatusCondition(
		f.proxyGroup("gateway").Status.Conditions, spawneryv1alpha1.ConditionChangingOver)
	if settled == nil || settled.Status != metav1.ConditionFalse {
		t.Fatalf("ChangingOver = %+v on a settled group, want False: a condition that is "+
			"never False cannot mark the transition into True either", settled)
	}
	if settled.Reason != spawneryv1alpha1.ReasonPodShapeCurrent {
		t.Errorf("reason = %q, want %q", settled.Reason, spawneryv1alpha1.ReasonPodShapeCurrent)
	}

	// Editing the label stands in for an operator upgrade moving the rendered shape; the
	// controller does not care which side moved.
	stale, ok := f.pod(pods[0].Name)
	if !ok {
		t.Fatalf("pod %s is gone", pods[0].Name)
	}
	stale.Labels[podspec.LabelPodHash] = "0000000000000000"
	if err := f.c.Update(f.ctx, stale); err != nil {
		t.Fatalf("age the pod's hash: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	rolling := meta.FindStatusCondition(
		f.proxyGroup("gateway").Status.Conditions, spawneryv1alpha1.ConditionChangingOver)
	if rolling == nil || rolling.Status != metav1.ConditionTrue {
		t.Fatalf("ChangingOver = %+v while a pod carries a shape the operator no longer "+
			"renders, want True", rolling)
	}
	if rolling.Reason != spawneryv1alpha1.ReasonPodShapeChanged {
		t.Errorf("reason = %q, want %q", rolling.Reason, spawneryv1alpha1.ReasonPodShapeChanged)
	}
	for _, want := range []string{"1 of", "operator upgrade"} {
		if !strings.Contains(rolling.Message, want) {
			t.Errorf("message = %q, want it to contain %q", rolling.Message, want)
		}
	}

	// It must clear on its own, or the condition latches.
	f.reconcileProxyGroup(r, "gateway")
	for _, p := range f.proxyPods("gateway") {
		if p.Labels[podspec.LabelPodHash] == "0000000000000000" {
			if err := f.c.Delete(f.ctx, &p); err != nil {
				t.Fatalf("delete the aged pod: %v", err)
			}
		}
	}
	f.reconcileProxyGroup(r, "gateway")

	cleared := meta.FindStatusCondition(
		f.proxyGroup("gateway").Status.Conditions, spawneryv1alpha1.ConditionChangingOver)
	if cleared == nil || cleared.Status != metav1.ConditionFalse {
		t.Errorf("ChangingOver = %+v once no pod carries an old shape, want False: a "+
			"condition that latches reports the upgrade forever", cleared)
	}
}
