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

// A LoadBalancer Service names no node port. One is allocated regardless, by
// the API server, and naming one here would add a second way for two groups
// in different namespaces to collide over a number no player ever dials.
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

// The CRD defaults externalTrafficPolicy to Local because bans and rate
// limits are built on the client's real IP, and Cluster SNATs it away. A
// ProxyGroup built in a unit test never passes through the API server's
// defaulting, so the default has to exist in the code as well as in the
// marker -- the same hazard podspec.DefaultDrainTimeoutSeconds exists for.
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

// Nothing inside the cluster dials a proxy: players arrive from outside,
// agents dial the operator, and Velocity dials backends. A Service left
// behind after a switch to HostPort would still hold its node port and still
// select the same pods, so the group would stay reachable by exactly the
// route the switch was meant to end.
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

// The operator deletes a Service because it owns it, not because it knows
// the name. A Service somebody else put at the group's name -- an ingress
// shim, a hand-written override -- is not this operator's to remove, and
// removing it would be an unrecoverable action taken on somebody else's
// object.
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

// spec.expose.loadBalancer.annotations is the only place a user writes into
// an object a third-party controller also writes into -- MetalLB and kube-vip
// both annotate the Service they act on. So the operator cannot treat the
// spec's map as the whole truth and delete whatever is not in it, and it
// cannot simply never delete either: a user who removes a pool annotation
// would see nothing happen, permanently, with no message anywhere. It
// records the keys it set and removes only those.
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

	// A third party annotates the Service the way a real load balancer
	// controller does. Nothing the operator does afterwards may remove it.
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

// A group that leaves LoadBalancer behind takes its annotations with it, and
// the bookkeeping key goes too -- otherwise the Service carries a record of
// keys nobody owns, and the next LoadBalancer group at that name inherits it.
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

// The API server's enum makes the false branch unreachable for any object
// that exists. The guard is here for the day a fifth value is added to the
// enum without a branch in reconcileService: a refusal on the object is a
// message a user can read, carried on the group where a user looks, while
// reconcileService's error reaches only the log.
//
// The older wording here promised a crash loop instead. That was true when
// reconcileService's default: arm fell through to reading NodePort.Port on a
// sub-block the CRD had never validated; this branch replaced that arm with
// an error naming the type, so the failure mode this guard is measured
// against is a named error, not a panic. The guard is worth keeping anyway:
// the two are not redundant, since only this one puts the refusal somewhere
// a user can read it.
//
// A pure function rather than an inline default arm because the enum is
// closed: no ProxyGroup carrying an unknown type can be created through
// envtest, so the branch is reachable from a test only here.
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

// The four strategies end to end, through Reconcile rather than through the
// pieces, because the refusal that stood in front of two of them is what this
// task removes.
//
// ClusterIP is here for a reason the other rows do not need stated. Every
// other ClusterIP test in this file calls reconcileService or proxyAddress
// directly, so without this row nothing drives exposeImplemented admitting
// the type and Reconcile reaching reconcileService for it.
//
// What this row does not cover is the rest of that path. It never reads
// status.address and cannot: no pod here is made ready, so proxyAddress
// returns "" for every row, and severing its ClusterIP arm leaves all four
// green -- measured. TestTheClusterIPAddressAppearsOnceAProxyIsReady is where
// that half is driven, and it is red under the same severance.
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

// A HostPort group in a namespace enforcing Pod Security baseline never gets
// a pod: the API server refuses the create outright. Before this, the error
// went to the log and the group reported Pending with no reason at all --
// for as long as the namespace's policy stood, which is forever.
//
// envtest runs the PodSecurity admission plugin, so the label below is
// enforced here exactly as it is in a cluster.
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

	// The reconcile returns the API server's error, so reconcileProxyGroup --
	// which fails the test on any error -- is the wrong helper here.
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
	// Both substrings, not just the first. PodSecurity alone would stay green
	// if the proxy pod acquired some unrelated baseline violation and the
	// container host port were dropped entirely -- and the host port is the
	// strategy under test. This pair, with its counterpart in
	// test/e2e/expose_test.go, is the whole evidentiary basis for the one
	// thing this milestone observed being enforced.
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

// TestAGroupSwitchedIntoARefusedStrategyStopsAdvertisingTheOldAddress is the
// scenario docs/known-issues.md's milestone 6c entry describes, driven rather
// than reasoned: a NodePort group publishing an address is switched to
// HostPort in a namespace that forbids host ports, reconcileService deletes
// the Service, the replacement pods are refused, and before this test existed
// Reconcile returned before setStatus and left status.address naming the node
// port of a Service that no longer existed -- for as long as the namespace
// label stood, which is forever.
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

	// Bring the group up and get an address on it.
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

	// Now forbid host ports and ask for them.
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

	// The premise: an old ready pod is still there. Without this, an empty
	// address might only mean "no pod is ready", which is a different and much
	// weaker statement than the one this test is making.
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
	// The empty address on its own would be its own defect: a group with no
	// address and no reason is indistinguishable from one that has not come up
	// yet.
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

// TestABrokenNetworkLeavesAWorkingAddressAlone pins the other half of the
// rule. Reconcile returns on a missing Network before it reads a single pod or
// Service, and the address must survive that: the proxies are still running,
// the Service is still there, and people are still connected through it. A
// deleted Network does not make an address wrong, and clearing it here would
// be a regression caused by the fix rather than a part of it.
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

	// This path returns cleanly with a requeue, so reconcileProxyGroup is the
	// right helper.
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

// TestAFailureInsideReconcileObservedLeavesTheAddressAlone is the other half
// of what TestABrokenNetworkLeavesAWorkingAddressAlone leaves unproven: that
// test drives a missing Network, which refuses before reconcileObserved is
// ever called, so obs.observed is never set at all. This drives a failure
// *inside* reconcileObserved -- one that happens after the group's Service
// and ready pod already exist -- and checks that obs.observed staying false
// still leaves status.address alone.
//
// The failure is reconcileService's own SetControllerReference call refusing
// to proceed: reconcileService ends with
// controllerutil.SetControllerReference(group, svc, r.Scheme), and that
// returns an AlreadyOwnedError once the Service already carries a controller
// owner reference naming something else. Giving the Service such a reference
// by hand touches nothing this test cares about keeping intact -- not the
// group, not its pods, not the Service's ports or selector -- and
// CreateOrUpdate aborts the mutate before ever calling Update, so the
// Service on the server is not changed by the failing pass either. The only
// effect is that reconcileService, and therefore reconcileObserved, returns
// an error before setting observed.
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

	// reconcileProxyGroup is the wrong helper here for the same reason it is
	// wrong in TestARejectedProxyPodIsReportedOnTheGroup: it fails the test on
	// any error, and an error is exactly what this pass must return.
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

// With hostPort the kube-scheduler places at most one pod of a group per
// node, so replicas is capped by the node count -- the likeliest HostPort
// mistake there is. The surplus pod exists and stays Pending, and the
// scheduler's own message on it is the only thing that explains why.
//
// envtest runs no scheduler, so the condition is written here the way one
// would write it. That is the honest shape of this test: it asserts the
// operator's reading of PodScheduled=False, not the scheduler's decision to
// set it.
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

// A group whose pods all exist says so, or the condition would latch True
// after any transient refusal and never come back.
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

// TestARecoveredProxyGroupFiresAnEventOnlyOnTheFlank pins the review's
// finding on Task 5: reportBlockedProxies's all-clear write must follow the
// same read-before/write/compare-after shape as ServerGroupReconciler's
// BackingOff/Degraded pair and this file's own reportReadinessDivergence, or
// a group recovering from a blocked proxy pod does so silently -- and a
// group that stays recovered must not repeat the event on every five-second
// resync the way a bare unconditional Eventf would.
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
	// Whatever fired getting the group to its first steady state is not what
	// this test is about.
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

	// What actually clears this in a cluster is the scheduler placing the
	// pod (or, on the rejected-create path, the namespace's policy
	// changing); here the pod's own condition is cleared directly, the same
	// honest-about-envtest shape TestAnUnschedulableProxyPodIsReportedOnTheGroup
	// already uses to get into the blocked state in the first place.
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

	// Steady state: nothing transitions on this pass, so nothing should
	// fire -- the whole reason every condition pair in this file is written
	// on the flank rather than on every resync.
	f.reconcileProxyGroup(r, "gateway")
	if steady := drainEvents(rec); containsEvent(steady, "ProxyPodsAdmitted") {
		t.Errorf("events = %v, want no ProxyPodsAdmitted on a pass where nothing changed", steady)
	}
}

// enforcePodSecurity labels the fixture's namespace so the API server's
// PodSecurity admission plugin enforces a profile on it. envtest runs that
// plugin, so this is the real control, not a stand-in for one.
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

// A refused create must leave the group's status alone on every pass after
// the first, and this test fails if it does not.
//
// The API server names the pod it refused, and NewProxyName draws a fresh
// random suffix for every attempt, so the refusal text differs on every pass.
// Storing it each time bumped resourceVersion; For(&ProxyGroup{}) carries no
// predicate, so the update event re-enqueued the group immediately, ahead of
// the rate-limited retry, and the group spun. The final review of 6c
// measured what that costs in a cluster: 3,940 refusals in a 139-second E2E
// run, where the backoff alone predicts about fifteen.
//
// The second half of this test is not decoration. The stored message has to
// stay the API server's own words -- the remedy is in them and nothing else
// knows it -- so a fix that stopped the churn by paraphrasing the refusal
// into words of its own would pass the resourceVersion assertion while
// breaking something worse. Both are asserted here for that reason.
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
		// Every pass fails: the namespace forbids host ports for as long as
		// its label stands, which is forever. reconcileProxyGroup fails the
		// test on any error, so it is the wrong helper here.
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

	// From the second pass on, nothing about the group changes: the first
	// pass is where the refusal is recorded, and every pass after it has
	// nothing new to say.
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

	// The stored text is the cluster's, verbatim: the object it names, the
	// verb the API server used, the policy that refused it, and the field
	// that violated it. Nothing here is this operator's wording.
	got := messages[len(messages)-1]
	for _, want := range []string{`pods "`, "is forbidden:", "PodSecurity", "hostPort"} {
		if !strings.Contains(got, want) {
			t.Errorf("message = %q, want it to contain %q -- the remedy is in the API "+
				"server's own words and nothing else knows it", got, want)
		}
	}
}

// sameRefusal is the one thing standing between "stop rewriting the object"
// and "report a stale remedy forever", so it is pinned on its own rather than
// only through the reconcile above.
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

// The delete guard has two halves and both are load-bearing, because
// cmd/spawnery-operator does not narrow the manager's cache for Services the
// way it does for ConfigMaps, ServiceAccounts and PVCs -- so this guard is
// the only thing between a stray object at the group's name and a delete
// that cannot be undone.
//
// TestSwitchingToHostPortLeavesAForeignServiceAlone above covers the Service
// with no owner at all. These are the two cases it does not: a Service
// controlled by a different object, and one carrying this group's controller
// reference without the operator's own label. Narrowing the guard to
// `owner == nil` left the whole package green before these existed.
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

		// A ProxyGroup of the same name that was deleted and recreated: the
		// old Service still carries the old object's UID, and the garbage
		// collector has not caught up with it yet. Same kind, same name,
		// different object -- which is exactly what the UID comparison is
		// for, and what a name comparison could never tell apart.
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

		// Built the way the operator builds it, so the owner reference is
		// genuinely this group's, and then stripped of the one other thing
		// design section 4 requires the guard to see.
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

// TestTheLoadBalancerAddressAppearsOnceAProxyIsReady is design section 12's
// third acceptance criterion, and the only place in this repository where the
// LoadBalancer address is driven through a live reconcile rather than through
// the pure function that computes it.
//
// envtest can do this precisely because it runs no kubelet and no load
// balancer controller: the test writes both halves itself -- the Service's
// ingress entry the way MetalLB would, and the pod's readiness the way a
// kubelet would -- and then asks the reconciler what it publishes. Nothing
// here pretends a load balancer ran, and nothing here says a client reached
// anything. What it proves is the wiring: reconcileService's returned Service
// reaching setStatus, and status.address coming out of it.
//
// The mutation that has to fail: severing that wiring in Reconcile, by
// passing setStatus a Service stripped of its status
// (`if svc != nil { svc = &corev1.Service{} }`). Before this test the whole
// package stayed green under it.
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

	// An assigned address alone publishes nothing. This is the same gate the
	// E2E's own LoadBalancer scenario is able to observe, restated here
	// because the pass that follows would otherwise prove only that some
	// address appeared, not that both conditions were required.
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

// The configured ClusterIP address reaches status.address through Reconcile,
// and only once a proxy is ready.
//
// TestReconcileAcceptsEveryStrategy's ClusterIP row drives the first half of
// that wiring -- exposeImplemented admitting the type, Reconcile reaching
// reconcileService for it -- and cannot drive this half: no pod in that table
// is ever made ready, so proxyAddress returns "" for every row there and an
// address assertion would pass just as well with the ClusterIP arm deleted.
// Making the pod ready is what gives the assertion something to fail on, and
// it is why this is a separate test rather than another column on the table:
// the other three rows would each need their own readiness and their own
// expected address, and LoadBalancer's would need an ingress written by hand.
//
// The mutation that has to fail, and was confirmed failing before this
// comment was written: return "" from proxyAddress's ClusterIP arm. That is
// the severance README.md records going unnoticed for LoadBalancer, where the
// Service sat disconnected from the status it is read out of and the package
// stayed green.
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

	// The gate matters more here than anywhere else: this is the one strategy
	// that already knows its whole answer before any pod exists, so without
	// the gate it would publish on the very first reconcile, for a group that
	// may never serve anyone.
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

// A ClusterIP group gets a Service the thing in front of it can route to, and
// nothing that reaches outside the cluster on its own. The absence of a node
// port is half the point: the strategy exists because the NodePort workaround
// left one allocated that nobody dialled and a firewall had to cover.
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

// A group moving from NodePort to ClusterIP was suspected of carrying two
// fields forward into a Service where neither belongs: externalTrafficPolicy,
// which the API server should refuse, and the allocated node port, which
// nothing would dial any more. Both come back cleared. The two halves are not
// the same kind of assertion, and this comment has been wrong about the
// difference twice, so it is worth being exact about what each one can
// detect.
//
// externalTrafficPolicy is the straightforward half. It lives on svc.Spec,
// the object CreateOrUpdate fetched before this arm ran, and the ClusterIP
// arm never assigns it -- so a stale Local really would go out on the wire
// unless something else stripped it. Measured against the envtest API server,
// Kubernetes v1.36.3: the API server normalises it away on the update to a
// type that does not support it. That half genuinely observes the API server,
// and would go red if the normalisation stopped.
//
// The node port is overdetermined, and that is the whole finding. Two
// independent mechanisms each force it to zero:
//
//   - reconcileService builds `port` as a fresh corev1.ServicePort literal
//     with no NodePort set and replaces svc.Spec.Ports wholesale in every arm,
//     so the update already carries nodePort: 0 and leaves nothing stale for
//     anyone to normalise;
//   - and the API server would clear it anyway, on the type change, if the
//     operator did send one.
//
// Either alone is sufficient, so this assertion is insensitive to both. It
// cannot go red unless the operator starts carrying a stale port forward AND
// the API server stops normalising. Both earlier versions of this comment
// picked one of the two and called it the mechanism under test; the honest
// answer is that the assertion does not distinguish them, and no reading of
// the source was ever going to settle which one "really" clears the field,
// because both do.
//
// The experiment is what ruled out the first story -- that this half guards
// reconcileService's own port reconstruction, so "a nonzero result means that
// reconstruction changed". Patch the ClusterIP arm to carry the stored port
// forward,
//
//	case spawneryv1alpha1.ExposeClusterIP:
//	        svc.Spec.Type = corev1.ServiceTypeClusterIP
//	        if len(svc.Spec.Ports) > 0 { port.NodePort = svc.Spec.Ports[0].NodePort }
//
// and this test still passes. Instrumented, the operator was seen sending
// NodePort=30001 and the stored Service still read 0, so the patch was doing
// what it looked like and the API server was undoing it. That kills the claim
// that the operator's reconstruction is what the assertion measures -- but it
// does not promote the API server into its place, which is the mistake the
// version after it made.
//
// Neither arm sets these fields explicitly, and that stays true. This
// repository's standing position is that a mechanism reporting nothing is
// indistinguishable from an absent one: an explicit `ExternalTrafficPolicy =
// ""` or `NodePort = 0` in the ClusterIP arm would be unfalsifiable, since no
// test could fail without it, and would be decoration wearing the shape of a
// fix.
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

// LoadBalancer -> ClusterIP releases exactly the annotations the operator set
// and leaves every foreign key alone. Milestone 6c built that mechanism;
// this is the first strategy to leave LoadBalancer for something other than
// NodePort.
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

// HostPort -> ClusterIP has to create a Service where the HostPort strategy
// deleted one.
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

// TestAGroupSaysWhenItsPodsNoLongerMatchWhatTheOperatorRenders is the outward
// half of docs/known-issues.md's milestone 4c-2 entry: "upgrading the operator
// can roll every proxy in the cluster, with nobody having edited a spec." The
// roll itself was already implemented and already correct. What was missing was
// any way to tell from the objects that it was happening — pods churning and
// readyReplicas dipping is what a dozen unrelated faults look like too.
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

	// What an operator upgrade does, reduced to its effect on one group: the
	// shape the operator renders moves, so the running pod's stamped hash stops
	// matching. Editing the label is how a unit-level test reaches that state
	// without rebuilding the operator — the controller compares the label
	// against what it renders now, and does not care which side moved.
	stale := &pods[0]
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
	// The count is the half an event could not carry, and the sentence about a
	// whole cluster saying this at once is the half a reader needs to tell an
	// upgrade from an edit.
	for _, want := range []string{"1 of", "operator upgrade"} {
		if !strings.Contains(rolling.Message, want) {
			t.Errorf("message = %q, want it to contain %q", rolling.Message, want)
		}
	}

	// It must clear on its own once the replacement carries the current shape,
	// or it would be a condition that latches and stops meaning anything.
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
