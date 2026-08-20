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
	"k8s.io/client-go/tools/events"
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
		{
			name: "ClusterIP publishes the configured address",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
			},
			pods: []corev1.Pod{readyPod()},
			want: "mc.example.test",
		},
		{
			// The address is a static string that needs no pod to compute, and
			// it is still withheld until one is ready. The column means the
			// same thing for all four strategies: you can connect here now.
			name: "ClusterIP publishes nothing until a proxy is ready",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
			},
			pods: []corev1.Pod{notReadyPod()},
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
		spawneryv1alpha1.ExposeClusterIP,
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
	rec := events.NewFakeRecorder(100)
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

// CreateOrUpdate mutates the Service it fetched, so a field the ClusterIP arm
// never sets keeps whatever a prior strategy left there. A group moving from
// NodePort -- whose arm sets externalTrafficPolicy to Local -- to ClusterIP
// was suspected of carrying that value forward into a Service the API server
// should refuse, since the field is meaningless there.
//
// It doesn't happen. Measured against the envtest API server, Kubernetes
// v1.36.3: with no explicit reset in reconcileService's ClusterIP arm, the
// field comes back empty anyway. The API server clears it itself on the
// update to a type that doesn't support it, before validation ever sees the
// stale value.
//
// That makes this a guard on the API server's behaviour, not the operator's
// -- and it stays that way on purpose. This repository's standing position
// is that a mechanism reporting nothing is indistinguishable from an absent
// one, and an explicit `ExternalTrafficPolicy = ""` in the ClusterIP arm is
// unfalsifiable: no test could ever fail without it, so it would be decoration
// wearing the shape of a fix. This test is the real guard instead. If it ever
// goes red, the API server's normalisation has changed or stopped, and the
// fix is to add that explicit reset to the ClusterIP arm -- the line this
// test exists so that nobody has to add speculatively today.
func TestTheAPIServerClearsExternalTrafficPolicyOnTheMoveToClusterIP(t *testing.T) {
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

	var svc corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %s, want ClusterIP", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want empty: the API server should have "+
			"cleared what the NodePort Service this group used to be left behind, "+
			"since reconcileService's ClusterIP arm never resets the field itself",
			svc.Spec.ExternalTrafficPolicy)
	}
}
