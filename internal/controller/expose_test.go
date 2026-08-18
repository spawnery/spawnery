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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// proxyAddress publishes an address only for a proxy that is demonstrably
// serving. The LoadBalancer branch is the one where that gate has to be
// stated rather than inherited: its address comes from the Service, which
// knows nothing about readiness, so without the gate status.address would
// point somewhere the moment a load balancer answered -- including for a
// group whose every pod is in ImagePullBackOff.
func TestProxyAddressPerStrategy(t *testing.T) {
	readyPod := func() corev1.Pod {
		return corev1.Pod{
			Status: corev1.PodStatus{
				HostIP: "10.0.0.7",
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
		}
	}
	notReadyPod := func() corev1.Pod {
		return corev1.Pod{
			Status: corev1.PodStatus{
				HostIP: "10.0.0.7",
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				},
			},
		}
	}
	withIngress := func(ing ...corev1.LoadBalancerIngress) *corev1.Service {
		return &corev1.Service{Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{Ingress: ing},
		}}
	}

	for _, tc := range []struct {
		name   string
		expose spawneryv1alpha1.ExposeSpec
		pods   []corev1.Pod
		svc    *corev1.Service
		want   string
	}{
		{
			name: "NodePort publishes the node's address",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  &corev1.Service{},
			want: "10.0.0.7:30001",
		},
		{
			name: "HostPort publishes the same node with the host port",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  nil,
			want: "10.0.0.7:25565",
		},
		{
			name: "LoadBalancer publishes the assigned ingress IP",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  withIngress(corev1.LoadBalancerIngress{IP: "192.0.2.10"}),
			want: "192.0.2.10:25565",
		},
		{
			name: "LoadBalancer falls back to the hostname",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  withIngress(corev1.LoadBalancerIngress{Hostname: "lb.example.net"}),
			want: "lb.example.net:25565",
		},
		{
			name: "LoadBalancer with an assigned address but no ready proxy",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{notReadyPod()},
			svc:  withIngress(corev1.LoadBalancerIngress{IP: "192.0.2.10"}),
			want: "",
		},
		{
			name: "LoadBalancer with a ready proxy and nothing assigned",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  withIngress(),
			want: "",
		},
		{
			name: "HostPort with no ready proxy",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			pods: []corev1.Pod{notReadyPod()},
			svc:  nil,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &spawneryv1alpha1.ProxyGroup{
				Spec: spawneryv1alpha1.ProxyGroupSpec{Expose: tc.expose},
			}
			if got := proxyAddress(group, tc.pods, tc.svc); got != tc.want {
				t.Errorf("proxyAddress = %q, want %q", got, tc.want)
			}
		})
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
// that exists. The guard is here for the day a fourth value is added to the
// enum without a branch in reconcileService: a refusal on the object is a
// message a user can read, and a nil dereference is a crash loop.
//
// A pure function rather than an inline default arm because the enum is
// closed: no ProxyGroup carrying an unknown type can be created through
// envtest, so the branch is reachable from a test only here.
func TestExposeImplementedCoversTheEnumAndNothingElse(t *testing.T) {
	for _, known := range []spawneryv1alpha1.ExposeType{
		spawneryv1alpha1.ExposeNodePort,
		spawneryv1alpha1.ExposeLoadBalancer,
		spawneryv1alpha1.ExposeHostPort,
	} {
		if !exposeImplemented(known) {
			t.Errorf("%s is in the CRD's enum, so a user can create a group asking for "+
				"it, and this operator refuses it", known)
		}
	}
	for _, unknown := range []spawneryv1alpha1.ExposeType{"", "Anycast", "nodeport"} {
		if exposeImplemented(unknown) {
			t.Errorf("%q is accepted as implemented; reconcileService has no branch for "+
				"it and would dereference a nil sub-block", unknown)
		}
	}
}

// The three strategies end to end, through Reconcile rather than through the
// pieces, because the refusal that stood in front of two of them is what this
// task removes.
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
