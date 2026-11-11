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

package agentendpoint_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/agentendpoint"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

const (
	serviceName = "spawnery-operator"
	leaseName   = "spawnery-operator.spawnery.cloud"
	agentPort   = 9443
)

type fixture struct {
	t   *testing.T
	ctx context.Context
	c   client.Client
	ns  string
	svc *corev1.Service
}

func newFixture(t *testing.T, holder string) *fixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "agent", Port: agentPort, Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := c.Create(ctx, svc); err != nil {
		t.Fatalf("create Service: %v", err)
	}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: ns},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}
	if err := c.Create(ctx, lease); err != nil {
		t.Fatalf("create Lease: %v", err)
	}
	return &fixture{t: t, ctx: ctx, c: c, ns: ns, svc: svc}
}

func (f *fixture) publisher(ips ...string) *agentendpoint.Publisher {
	return &agentendpoint.Publisher{
		Client:    f.c,
		Namespace: f.ns,
		Service:   serviceName,
		Port:      agentPort,
		Lease:     leaseName,
		Pod: agentendpoint.Identity{
			Name: "op-a", Hostname: "host-a", UID: "uid-a", Node: "n1", IPs: ips,
		},
	}
}

func (f *fixture) slice(family discoveryv1.AddressType) (*discoveryv1.EndpointSlice, error) {
	s := &discoveryv1.EndpointSlice{}
	err := f.c.Get(f.ctx, types.NamespacedName{
		Namespace: f.ns, Name: agentendpoint.SliceName(serviceName, family),
	}, s)
	return s, err
}

func (f *fixture) mustSlice(family discoveryv1.AddressType) *discoveryv1.EndpointSlice {
	f.t.Helper()
	s, err := f.slice(family)
	if err != nil {
		f.t.Fatalf("get %s slice: %v", family, err)
	}
	return s
}

func (f *fixture) slices() []discoveryv1.EndpointSlice {
	f.t.Helper()
	list := &discoveryv1.EndpointSliceList{}
	if err := f.c.List(f.ctx, list, client.InNamespace(f.ns)); err != nil {
		f.t.Fatalf("list slices: %v", err)
	}
	return list.Items
}

func (f *fixture) assert(p *agentendpoint.Publisher) {
	f.t.Helper()
	if err := p.Assert(f.ctx); err != nil {
		f.t.Fatalf("Assert: %v", err)
	}
}

func (f *fixture) foreignSlice(service, managedBy, ip string) *discoveryv1.EndpointSlice {
	f.t.Helper()
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: service + "-",
			Namespace:    f.ns,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: service,
				discoveryv1.LabelManagedBy:   managedBy,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr.To("agent"), Port: ptr.To[int32](agentPort), Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
	if err := f.c.Create(f.ctx, s); err != nil {
		f.t.Fatalf("create slice: %v", err)
	}
	return s
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func checkSlice(t *testing.T, s *discoveryv1.EndpointSlice, svc *corev1.Service, family discoveryv1.AddressType, ip string) {
	t.Helper()
	if s.AddressType != family {
		t.Errorf("%s: addressType %s", s.Name, s.AddressType)
	}
	if got := s.Labels[discoveryv1.LabelServiceName]; got != serviceName {
		t.Errorf("%s: service-name label %q", s.Name, got)
	}
	if got := s.Labels[discoveryv1.LabelManagedBy]; got != agentendpoint.ManagedBy {
		t.Errorf("%s: managed-by label %q", s.Name, got)
	}
	if len(s.OwnerReferences) != 1 {
		t.Errorf("%s: owner references %+v, want the Service alone", s.Name, s.OwnerReferences)
	} else if o := s.OwnerReferences[0]; o.Kind != "Service" || o.APIVersion != "v1" || o.Name != serviceName || o.UID != svc.UID {
		t.Errorf("%s: owner reference %+v, want Service %s/%s", s.Name, o, serviceName, svc.UID)
	}
	if len(s.Endpoints) != 1 {
		t.Fatalf("%s: %d endpoints, want 1", s.Name, len(s.Endpoints))
	}
	ep := s.Endpoints[0]
	if len(ep.Addresses) != 1 || ep.Addresses[0] != ip {
		t.Errorf("%s: addresses %v, want [%s]", s.Name, ep.Addresses, ip)
	}
	if ep.Conditions.Ready == nil || !*ep.Conditions.Ready {
		t.Errorf("%s: endpoint not ready", s.Name)
	}
	if ep.TargetRef == nil || ep.TargetRef.Kind != "Pod" || ep.TargetRef.Name != "op-a" || ep.TargetRef.UID != "uid-a" {
		t.Errorf("%s: targetRef %+v, want pod op-a uid-a", s.Name, ep.TargetRef)
	}
	if ep.NodeName == nil || *ep.NodeName != "n1" {
		t.Errorf("%s: nodeName %v, want n1", s.Name, ep.NodeName)
	}
	if len(s.Ports) != 1 {
		t.Fatalf("%s: ports %+v, want one", s.Name, s.Ports)
	}
	p := s.Ports[0]
	if p.Name == nil || *p.Name != "agent" || p.Port == nil || *p.Port != agentPort || p.Protocol == nil || *p.Protocol != corev1.ProtocolTCP {
		t.Errorf("%s: port %+v, want agent/%d/TCP", s.Name, p, agentPort)
	}
}

func TestAssertWritesOneSlicePerFamily(t *testing.T) {
	f := newFixture(t, "host-a_123")
	f.assert(f.publisher("10.0.0.5", "fd00::5"))

	if got := agentendpoint.SliceName(serviceName, discoveryv1.AddressTypeIPv4); got != "spawnery-operator-agent-ipv4" {
		t.Errorf("SliceName IPv4 = %q", got)
	}
	if got := agentendpoint.SliceName(serviceName, discoveryv1.AddressTypeIPv6); got != "spawnery-operator-agent-ipv6" {
		t.Errorf("SliceName IPv6 = %q", got)
	}
	checkSlice(t, f.mustSlice(discoveryv1.AddressTypeIPv4), f.svc, discoveryv1.AddressTypeIPv4, "10.0.0.5")
	checkSlice(t, f.mustSlice(discoveryv1.AddressTypeIPv6), f.svc, discoveryv1.AddressTypeIPv6, "fd00::5")
	if n := len(f.slices()); n != 2 {
		t.Errorf("%d slices in the namespace, want 2", n)
	}
}

func TestASingleStackPodGetsOneSlice(t *testing.T) {
	f := newFixture(t, "host-a_123")
	f.assert(f.publisher("10.0.0.5", "fd00::5"))
	f.assert(f.publisher("10.0.0.5"))

	checkSlice(t, f.mustSlice(discoveryv1.AddressTypeIPv4), f.svc, discoveryv1.AddressTypeIPv4, "10.0.0.5")
	if _, err := f.slice(discoveryv1.AddressTypeIPv6); !apierrors.IsNotFound(err) {
		t.Errorf("IPv6 slice: err %v, want NotFound", err)
	}
}

func TestAssertReplacesASliceNamingAnotherPod(t *testing.T) {
	f := newFixture(t, "host-a_123")
	stale := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentendpoint.SliceName(serviceName, discoveryv1.AddressTypeIPv4),
			Namespace: f.ns,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: serviceName,
				discoveryv1.LabelManagedBy:   agentendpoint.ManagedBy,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.7"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: f.ns, Name: "op-b", UID: "uid-b"},
			NodeName:   ptr.To("n2"),
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr.To("agent"), Port: ptr.To[int32](agentPort), Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
	if err := f.c.Create(f.ctx, stale); err != nil {
		t.Fatalf("create stale slice: %v", err)
	}

	f.assert(f.publisher("10.0.0.5"))

	checkSlice(t, f.mustSlice(discoveryv1.AddressTypeIPv4), f.svc, discoveryv1.AddressTypeIPv4, "10.0.0.5")
}

func TestAssertRecreatesADeletedSlice(t *testing.T) {
	f := newFixture(t, "host-a_123")
	p := f.publisher("10.0.0.5")
	f.assert(p)
	if err := f.c.Delete(f.ctx, f.mustSlice(discoveryv1.AddressTypeIPv4)); err != nil {
		t.Fatalf("delete slice: %v", err)
	}
	if _, err := f.slice(discoveryv1.AddressTypeIPv4); !apierrors.IsNotFound(err) {
		t.Fatalf("slice still there after delete: %v", err)
	}

	f.assert(p)

	checkSlice(t, f.mustSlice(discoveryv1.AddressTypeIPv4), f.svc, discoveryv1.AddressTypeIPv4, "10.0.0.5")
}

func TestAssertRemovesWhatTheSelectorLeftBehind(t *testing.T) {
	f := newFixture(t, "host-a_123")
	controllerSlice := f.foreignSlice(serviceName, "endpointslice-controller.k8s.io", "10.0.0.9")
	otherService := f.foreignSlice("other", "endpointslice-controller.k8s.io", "10.0.0.10")
	endpoints := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Endpoints",
		"metadata":   map[string]any{"name": serviceName, "namespace": f.ns},
		"subsets": []any{map[string]any{
			"addresses": []any{map[string]any{"ip": "10.0.0.9"}},
			"ports":     []any{map[string]any{"name": "agent", "port": int64(agentPort), "protocol": "TCP"}},
		}},
	}}
	if err := f.c.Create(f.ctx, endpoints); err != nil {
		t.Fatalf("create Endpoints: %v", err)
	}

	f.assert(f.publisher("10.0.0.5"))

	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(controllerSlice), &discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(err) {
		t.Errorf("the EndpointSlice controller's slice: err %v, want NotFound", err)
	}
	gone := &unstructured.Unstructured{}
	gone.SetAPIVersion("v1")
	gone.SetKind("Endpoints")
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(endpoints), gone); !apierrors.IsNotFound(err) {
		t.Errorf("Endpoints %s: err %v, want NotFound", serviceName, err)
	}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(otherService), &discoveryv1.EndpointSlice{}); err != nil {
		t.Errorf("another Service's slice: %v, want it left alone", err)
	}
	checkSlice(t, f.mustSlice(discoveryv1.AddressTypeIPv4), f.svc, discoveryv1.AddressTypeIPv4, "10.0.0.5")
}

func TestAssertDoesNothingWithoutTheLease(t *testing.T) {
	for _, holder := range []string{"host-b_456", "host-ab_789"} {
		t.Run(holder, func(t *testing.T) {
			f := newFixture(t, holder)
			left := f.foreignSlice(serviceName, "endpointslice-controller.k8s.io", "10.0.0.9")

			err := f.publisher("10.0.0.5", "fd00::5").Assert(f.ctx)

			if !errors.Is(err, agentendpoint.ErrNotHolder) {
				t.Fatalf("Assert: err %v, want ErrNotHolder", err)
			}
			slices := f.slices()
			if len(slices) != 1 || slices[0].Name != left.Name {
				t.Errorf("slices after a refused Assert: %d, want only the one left behind", len(slices))
			}
		})
	}
}

func TestAssertWithoutALeaseNameChecksNoHolder(t *testing.T) {
	f := newFixture(t, "host-b_456")
	p := f.publisher("10.0.0.5")
	p.Lease = ""

	f.assert(p)

	checkSlice(t, f.mustSlice(discoveryv1.AddressTypeIPv4), f.svc, discoveryv1.AddressTypeIPv4, "10.0.0.5")
}

func TestWithdrawEmptiesItsOwnSlice(t *testing.T) {
	f := newFixture(t, "host-a_123")
	p := f.publisher("10.0.0.5", "fd00::5")
	f.assert(p)

	if err := p.Withdraw(f.ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	for _, family := range []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4, discoveryv1.AddressTypeIPv6} {
		if s := f.mustSlice(family); len(s.Endpoints) != 0 {
			t.Errorf("%s: endpoints %+v after Withdraw, want none", family, s.Endpoints)
		}
	}
}

func TestWithdrawLeavesAnotherPodsSliceAlone(t *testing.T) {
	f := newFixture(t, "host-a_123")
	p := f.publisher("10.0.0.5")
	f.assert(p)
	s := f.mustSlice(discoveryv1.AddressTypeIPv4)
	s.Endpoints[0].Addresses = []string{"10.0.0.7"}
	s.Endpoints[0].TargetRef.Name = "op-b"
	s.Endpoints[0].TargetRef.UID = "uid-b"
	if err := f.c.Update(f.ctx, s); err != nil {
		t.Fatalf("rewrite the slice to another pod: %v", err)
	}

	if err := p.Withdraw(f.ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	after := f.mustSlice(discoveryv1.AddressTypeIPv4)
	if len(after.Endpoints) != 1 || after.Endpoints[0].TargetRef == nil || after.Endpoints[0].TargetRef.UID != "uid-b" {
		t.Errorf("endpoints %+v after Withdraw, want the one naming uid-b", after.Endpoints)
	}
}

func TestStartWaitsForTheListener(t *testing.T) {
	f := newFixture(t, "host-a_123")
	serving := make(chan struct{})
	p := f.publisher("10.0.0.5")
	p.Serving = serving

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	time.Sleep(time.Second)
	if _, err := f.slice(discoveryv1.AddressTypeIPv4); !apierrors.IsNotFound(err) {
		t.Fatalf("a slice before the listener is bound: err %v, want NotFound", err)
	}

	close(serving)
	eventually(t, 2*time.Second, "the slice after Serving", func() bool {
		s, err := f.slice(discoveryv1.AddressTypeIPv4)
		return err == nil && len(s.Endpoints) == 1
	})

	cancel()
	eventually(t, 3*time.Second, "the emptied slice after cancel", func() bool {
		s, err := f.slice(discoveryv1.AddressTypeIPv4)
		return err == nil && len(s.Endpoints) == 0
	})
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Start did not return after cancel")
	}
}

func TestParseIPs(t *testing.T) {
	ips, err := agentendpoint.ParseIPs("10.0.0.5,fd00::5")
	if err != nil {
		t.Fatalf("ParseIPs: %v", err)
	}
	if len(ips) != 2 || ips[0] != "10.0.0.5" || ips[1] != "fd00::5" {
		t.Errorf("ParseIPs = %v, want [10.0.0.5 fd00::5]", ips)
	}
	for _, bad := range []string{"", "nope"} {
		if ips, err := agentendpoint.ParseIPs(bad); err == nil {
			t.Errorf("ParseIPs(%q) = %v, want an error", bad, ips)
		}
	}
}
