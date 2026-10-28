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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/voice"
)

func (f *fixture) enableVoice(t *testing.T) {
	t.Helper()
	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network: %v", err)
	}
	net.Spec.Voice = &spawneryv1alpha1.NetworkVoiceSpec{}
	if err := f.c.Update(f.ctx, net); err != nil {
		t.Fatalf("turn voice on in the network: %v", err)
	}
}

func (f *fixture) service(t *testing.T, name string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get Service %s: %v", name, err)
	}
	return svc
}

func exposeLoadBalancer(g *spawneryv1alpha1.ProxyGroup) {
	g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:         spawneryv1alpha1.ExposeLoadBalancer,
		LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
	}
}

func voiceOverride(spec spawneryv1alpha1.ExposeSpec) func(*spawneryv1alpha1.ProxyGroup) {
	return func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: &spec}
	}
}

func assertOnlyTCP(t *testing.T, svc *corev1.Service) {
	t.Helper()
	if svc == nil {
		t.Fatal("the game Service does not exist")
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP ||
		svc.Spec.Ports[0].Name != podspec.MinecraftPortName {
		t.Fatalf("game Service ports = %+v, want exactly minecraft/TCP", svc.Spec.Ports)
	}
}

func TestAVoiceNetworkAddsUDPToTheGamesService(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer)

	f.reconcileProxyGroup(r, "gateway")

	svc := f.service(t, "gateway")
	if svc == nil {
		t.Fatal("the LoadBalancer group got no Service")
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("ports = %+v, want minecraft/TCP and voice/UDP", svc.Spec.Ports)
	}
	game, udp := svc.Spec.Ports[0], svc.Spec.Ports[1]
	if game.Name != podspec.MinecraftPortName || game.Protocol != corev1.ProtocolTCP || game.Port != podspec.MinecraftPort {
		t.Errorf("first port = %+v, want minecraft/TCP %d", game, podspec.MinecraftPort)
	}
	if udp.Name != podspec.VoicePortName || udp.Protocol != corev1.ProtocolUDP || udp.Port != voice.RelayPort ||
		udp.TargetPort != intstr.FromString(podspec.VoicePortName) {
		t.Errorf("second port = %+v, want voice/UDP %d targeting %q", udp, voice.RelayPort, podspec.VoicePortName)
	}
	if other := f.service(t, podspec.VoiceServiceName("gateway")); other != nil {
		t.Errorf("voice takes the game's way out, yet %s exists: %+v", other.Name, other.Spec)
	}
	group := f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice,
		metav1.ConditionUnknown, spawneryv1alpha1.ReasonVoiceRelayNotReported) {
		t.Errorf("conditions = %+v, want Voice=Unknown/RelayNotReported before any proxy reports", group.Status.Conditions)
	}

	connectProxiesReporting(f, "gateway", agent.VoiceRelayRunning)
	f.reconcileProxyGroup(r, "gateway")

	group = f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonVoiceRelaying) {
		t.Errorf("conditions = %+v, want Voice=True/Relaying once the proxies report it running", group.Status.Conditions)
	}
}

func TestANodePortGroupSharesItsNodePortAcrossProtocols(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose.NodePort = &spawneryv1alpha1.NodePortSpec{Port: 30781}
	})

	f.reconcileProxyGroup(r, "gateway")

	svc := f.service(t, "gateway")
	if svc == nil || len(svc.Spec.Ports) != 2 {
		t.Fatalf("Service = %+v, want two ports", svc)
	}
	for _, p := range svc.Spec.Ports {
		if p.NodePort != 30781 {
			t.Errorf("port %s/%s has node port %d, want 30781 for both protocols", p.Name, p.Protocol, p.NodePort)
		}
	}
}

func TestWithoutVoiceTheServiceIsUnchanged(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer)

	f.reconcileProxyGroup(r, "gateway")

	assertOnlyTCP(t, f.service(t, "gateway"))
	if other := f.service(t, podspec.VoiceServiceName("gateway")); other != nil {
		t.Errorf("%s exists in a network without voice", other.Name)
	}
	group := f.proxyGroup("gateway")
	if c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice); c != nil {
		t.Errorf("Voice condition %+v on a group in a network without voice", *c)
	}
	if group.Status.VoiceAddress != "" {
		t.Errorf("status.voiceAddress = %q, want empty", group.Status.VoiceAddress)
	}
}

func TestAVoiceOverrideGetsItsOwnServiceAndLeavesTheGameAlone(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30782},
	}))

	f.reconcileProxyGroup(r, "gateway")

	assertOnlyTCP(t, f.service(t, "gateway"))
	vs := f.service(t, podspec.VoiceServiceName("gateway"))
	if vs == nil {
		t.Fatal("the NodePort override got no voice Service")
	}
	if vs.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("voice Service type = %q, want NodePort", vs.Spec.Type)
	}
	if vs.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("voice Service externalTrafficPolicy = %q, want Local", vs.Spec.ExternalTrafficPolicy)
	}
	if len(vs.Spec.Ports) != 1 || vs.Spec.Ports[0].Protocol != corev1.ProtocolUDP ||
		vs.Spec.Ports[0].Name != podspec.VoicePortName || vs.Spec.Ports[0].NodePort != 30782 {
		t.Errorf("voice Service ports = %+v, want one voice/UDP port on node port 30782", vs.Spec.Ports)
	}
	if owner := metav1.GetControllerOf(vs); owner == nil || owner.UID != group.UID {
		t.Errorf("voice Service controller = %+v, want the group", owner)
	}
}

func TestDroppingTheOverrideDeletesTheVoiceService(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30783},
	}))
	f.reconcileProxyGroup(r, "gateway")
	if f.service(t, podspec.VoiceServiceName("gateway")) == nil {
		t.Fatal("the override's voice Service was never created")
	}

	group := f.proxyGroup("gateway")
	group.Spec.Voice.Expose = nil
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("drop spec.voice.expose: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	if vs := f.service(t, podspec.VoiceServiceName("gateway")); vs != nil {
		t.Errorf("the voice Service survived dropping the override: %+v", vs.Spec)
	}
	svc := f.service(t, "gateway")
	if svc == nil || len(svc.Spec.Ports) != 2 || svc.Spec.Ports[1].Protocol != corev1.ProtocolUDP {
		t.Errorf("game Service = %+v, want it to gain the UDP port", svc)
	}
}

func TestAForeignVoiceServiceIsReportedAndLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.VoiceServiceName("gateway"), Namespace: f.ns},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "other", Port: 8080, Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create the foreign Service: %v", err)
	}
	t.Cleanup(func() { _ = f.c.Delete(f.ctx, foreign) })
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type: spawneryv1alpha1.ExposeLoadBalancer,
	}))

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonVoiceServiceNotOurs) {
		t.Errorf("conditions = %+v, want Voice=False/%s", group.Status.Conditions,
			spawneryv1alpha1.ReasonVoiceServiceNotOurs)
	}
	if group.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", group.Status.Phase)
	}
	after := f.service(t, foreign.Name)
	if after == nil || after.ResourceVersion != foreign.ResourceVersion {
		t.Errorf("the foreign Service was touched: before rv %s, after %+v", foreign.ResourceVersion, after)
	}
	assertOnlyTCP(t, f.service(t, "gateway"))
}

func TestGroupVoiceWithoutNetworkVoiceIsReported(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30784},
	}))

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonVoiceOffInNetwork) {
		t.Errorf("conditions = %+v, want Voice=False/%s", group.Status.Conditions,
			spawneryv1alpha1.ReasonVoiceOffInNetwork)
	}
	assertOnlyTCP(t, f.service(t, "gateway"))
	if vs := f.service(t, podspec.VoiceServiceName("gateway")); vs != nil {
		t.Errorf("a voice Service exists in a network without voice: %+v", vs.Spec)
	}
}

func TestAHostPortOverrideOutsideTheRangeIsRefused(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	f.allowScheduling(t, &spawneryv1alpha1.SchedulingPolicy{
		HostPortRange: &spawneryv1alpha1.PortRange{Min: 25000, Max: 25100},
	})
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 26000},
	}))

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != spawneryv1alpha1.ReasonHostPortNotAllowed {
		t.Fatalf("Accepted = %+v, want False/%s", c, spawneryv1alpha1.ReasonHostPortNotAllowed)
	}
	if !strings.HasPrefix(c.Message, "spec.voice.expose: ") {
		t.Errorf("message = %q, want it to start with %q", c.Message, "spec.voice.expose: ")
	}
}

func (f *fixture) disableVoice(t *testing.T) {
	t.Helper()
	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network: %v", err)
	}
	net.Spec.Voice = nil
	if err := f.c.Update(f.ctx, net); err != nil {
		t.Fatalf("turn voice off in the network: %v", err)
	}
}

func (f *fixture) setVoiceOverride(t *testing.T, name string, spec *spawneryv1alpha1.ExposeSpec) {
	t.Helper()
	group := f.proxyGroup(name)
	group.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: spec}
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("set spec.voice.expose of %s: %v", name, err)
	}
}

func TestAVoiceServiceWithoutTheManagedByLabelIsNotOurs(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30785},
	}))
	f.reconcileProxyGroup(r, "gateway")
	vs := f.service(t, podspec.VoiceServiceName("gateway"))
	if vs == nil {
		t.Fatal("the override's voice Service was never created")
	}
	delete(vs.Labels, podspec.LabelManagedBy)
	if err := f.c.Update(f.ctx, vs); err != nil {
		t.Fatalf("strip the managed-by label: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")
	group := f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonVoiceServiceNotOurs) {
		t.Errorf("conditions = %+v, want Voice=False/%s while the label is gone", group.Status.Conditions,
			spawneryv1alpha1.ReasonVoiceServiceNotOurs)
	}
	if after := f.service(t, vs.Name); after == nil || after.ResourceVersion != vs.ResourceVersion {
		t.Errorf("the unlabelled voice Service was rewritten: before rv %s, after %+v", vs.ResourceVersion, after)
	}

	f.setVoiceOverride(t, "gateway", nil)
	f.reconcileProxyGroup(r, "gateway")
	if after := f.service(t, vs.Name); after == nil || after.ResourceVersion != vs.ResourceVersion {
		t.Errorf("the unlabelled voice Service was deleted or rewritten once unwanted: %+v", after)
	}
}

func TestRetypingTheVoiceOverrideLeavesNothingOfTheOldType(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30786},
	}))
	f.reconcileProxyGroup(r, "gateway")
	name := podspec.VoiceServiceName("gateway")
	if vs := f.service(t, name); vs == nil || vs.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("voice Service = %+v, want NodePort", vs)
	}

	f.setVoiceOverride(t, "gateway", &spawneryv1alpha1.ExposeSpec{
		Type: spawneryv1alpha1.ExposeLoadBalancer,
		LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
			Annotations:           map[string]string{"lb.example.test/pool": "voice"},
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
		},
	})
	f.reconcileProxyGroup(r, "gateway")
	vs := f.service(t, name)
	if vs == nil || vs.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("voice Service = %+v, want LoadBalancer", vs)
	}
	if vs.Annotations["lb.example.test/pool"] != "voice" {
		t.Errorf("annotations = %v, want the override's lb.example.test/pool", vs.Annotations)
	}
	if vs.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Errorf("externalTrafficPolicy = %q, want the override's Cluster", vs.Spec.ExternalTrafficPolicy)
	}

	f.setVoiceOverride(t, "gateway", &spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "voice.example.test:24000"},
	})
	f.reconcileProxyGroup(r, "gateway")
	vs = f.service(t, name)
	if vs == nil || vs.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("voice Service = %+v, want ClusterIP", vs)
	}
	if _, kept := vs.Annotations["lb.example.test/pool"]; kept {
		t.Errorf("annotations = %v, want the LoadBalancer's key released", vs.Annotations)
	}
	if vs.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want none on ClusterIP", vs.Spec.ExternalTrafficPolicy)
	}
	if len(vs.Spec.Ports) != 1 || vs.Spec.Ports[0].NodePort != 0 {
		t.Errorf("ports = %+v, want one port without a node port", vs.Spec.Ports)
	}
}

func TestTurningNetworkVoiceOffTakesTheUDPAwayEverywhere(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30787},
	}))
	f.createProxyGroup("hub", exposeLoadBalancer)
	f.reconcileProxyGroup(r, "gateway")
	f.reconcileProxyGroup(r, "hub")
	if f.service(t, podspec.VoiceServiceName("gateway")) == nil {
		t.Fatal("the override's voice Service was never created")
	}
	if svc := f.service(t, "hub"); svc == nil || len(svc.Spec.Ports) != 2 {
		t.Fatalf("hub Service = %+v, want the UDP port beside the game's", svc)
	}

	f.disableVoice(t)
	f.reconcileProxyGroup(r, "gateway")
	f.reconcileProxyGroup(r, "hub")

	if vs := f.service(t, podspec.VoiceServiceName("gateway")); vs != nil {
		t.Errorf("the voice Service survived the network turning voice off: %+v", vs.Spec)
	}
	assertOnlyTCP(t, f.service(t, "gateway"))
	assertOnlyTCP(t, f.service(t, "hub"))
}

func TestAForeignVoiceServiceIsLeftAloneWhenNoneIsWanted(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.VoiceServiceName("gateway"), Namespace: f.ns},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "other", Port: 8080, Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create the foreign Service: %v", err)
	}
	t.Cleanup(func() { _ = f.c.Delete(f.ctx, foreign) })
	f.createProxyGroup("gateway", exposeLoadBalancer)

	f.reconcileProxyGroup(r, "gateway")
	connectProxiesReporting(f, "gateway", agent.VoiceRelayRunning)
	f.reconcileProxyGroup(r, "gateway")

	if after := f.service(t, foreign.Name); after == nil || after.ResourceVersion != foreign.ResourceVersion {
		t.Errorf("the foreign Service was touched: before rv %s, after %+v", foreign.ResourceVersion, after)
	}
	group := f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonVoiceRelaying) {
		t.Errorf("conditions = %+v, want Voice=True: voice takes the game's way and needs no Service of its own",
			group.Status.Conditions)
	}
}

func TestAVoiceRefusalAfterAGameRefusalIsAnnounced(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	f.allowScheduling(t, &spawneryv1alpha1.SchedulingPolicy{
		HostPortRange: &spawneryv1alpha1.PortRange{Min: 25000, Max: 25100},
	})
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 26000},
		}
	})
	f.reconcileProxyGroup(r, "gateway")
	if n := countEvents(drainEvents(rec), spawneryv1alpha1.ReasonHostPortNotAllowed); n != 1 {
		t.Fatalf("%d HostPortNotAllowed events for the game's refusal, want 1", n)
	}

	group := f.proxyGroup("gateway")
	group.Spec.Expose.HostPort.Port = 25050
	group.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: &spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 26001},
	}}
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("move the refusal from the game to voice: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")
	events := drainEvents(rec)
	if n := countEvents(events, spawneryv1alpha1.ReasonHostPortNotAllowed); n != 1 {
		t.Fatalf("events = %q, want one HostPortNotAllowed for the voice refusal", events)
	}
	if !strings.Contains(events[0], "spec.voice.expose: ") {
		t.Errorf("event = %q, want it to name spec.voice.expose", events[0])
	}

	f.reconcileProxyGroup(r, "gateway")
	if events := drainEvents(rec); countEvents(events, spawneryv1alpha1.ReasonHostPortNotAllowed) != 0 {
		t.Errorf("events = %q, want none for an unchanged refusal", events)
	}
}

func TestARejectedVoiceServiceCostsVoiceOnly(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	squatter := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "squatter", Namespace: f.ns},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Name: "other", Port: 8080, Protocol: corev1.ProtocolUDP,
				NodePort: 30788}},
		},
	}
	if err := f.c.Create(f.ctx, squatter); err != nil {
		t.Fatalf("create the Service holding node port 30788: %v", err)
	}
	t.Cleanup(func() { _ = f.c.Delete(f.ctx, squatter) })
	f.createProxyGroup("gateway", exposeLoadBalancer, voiceOverride(spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30788},
	}))

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != spawneryv1alpha1.ReasonVoiceServiceRejected {
		t.Fatalf("Voice = %+v, want False/%s", c, spawneryv1alpha1.ReasonVoiceServiceRejected)
	}
	if !strings.Contains(c.Message, "already allocated") {
		t.Errorf("message = %q, want the API server's refusal", c.Message)
	}
	if group.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", group.Status.Phase)
	}
	assertOnlyTCP(t, f.service(t, "gateway"))
	if pods := f.proxyPods("gateway"); len(pods) == 0 {
		t.Error("no proxy pods: the voice Service's refusal stopped the game's reconcile")
	}
}

func TestAProxyWithItsOwnVoicePluginIsReported(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer)
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("the group has no proxy pods")
	}
	uid := string(pods[0].UID)
	f.agents.Connect(uid, agent.RoleProxy)
	f.agents.ReportVoicePlugin(uid, true)

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != spawneryv1alpha1.ReasonVoicePluginConflict {
		t.Fatalf("Voice = %+v, want False/%s", c, spawneryv1alpha1.ReasonVoicePluginConflict)
	}
	if !strings.Contains(c.Message, pods[0].Name) {
		t.Errorf("message = %q, want it to name %s", c.Message, pods[0].Name)
	}
	if group.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", group.Status.Phase)
	}
}

func connectProxiesReporting(f *fixture, group string, state agent.VoiceRelayState) []corev1.Pod {
	f.t.Helper()
	pods := f.proxyPods(group)
	if len(pods) == 0 {
		f.t.Fatalf("group %s has no proxy pods", group)
	}
	for i := range pods {
		uid := string(pods[i].UID)
		f.agents.Connect(uid, agent.RoleProxy)
		f.agents.ReportVoiceRelay(uid, state)
	}
	return pods
}

func TestAProxyWhoseRelayCouldNotBindFailsTheVoiceCondition(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer)
	f.reconcileProxyGroup(r, "gateway")

	pods := connectProxiesReporting(f, "gateway", agent.VoiceRelayRunning)
	f.agents.ReportVoiceRelay(string(pods[0].UID), agent.VoiceRelayBindFailed)
	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != spawneryv1alpha1.ReasonVoiceRelayBindFailed {
		t.Fatalf("Voice = %+v, want False/%s", c, spawneryv1alpha1.ReasonVoiceRelayBindFailed)
	}
	if !strings.Contains(c.Message, pods[0].Name) {
		t.Errorf("message = %q, want it to name %s", c.Message, pods[0].Name)
	}
	if group.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", group.Status.Phase)
	}

	f.agents.ReportVoiceRelay(string(pods[0].UID), agent.VoiceRelayRunning)
	f.reconcileProxyGroup(r, "gateway")
	if c := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions, spawneryv1alpha1.ConditionVoice); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Errorf("Voice = %+v after the retry bound, want True", c)
	}
}

func TestAProxyThatDoesNotReportItsRelayReadsAsUnknownNotAsAFailure(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer)
	f.reconcileProxyGroup(r, "gateway")

	pods := connectProxiesReporting(f, "gateway", agent.VoiceRelayRunning)
	f.agents.ReportVoiceRelay(string(pods[0].UID), agent.VoiceRelayNotReported)
	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionVoice)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != spawneryv1alpha1.ReasonVoiceRelayNotReported {
		t.Fatalf("Voice = %+v, want Unknown/%s", c, spawneryv1alpha1.ReasonVoiceRelayNotReported)
	}
	if !strings.Contains(c.Message, pods[0].Name) {
		t.Errorf("message = %q, want it to name %s", c.Message, pods[0].Name)
	}
	if group.Status.Phase == "Degraded" {
		t.Errorf("phase = Degraded: a proxy that cannot report is not a failure")
	}
}

func TestAProxyWhoseStreamIsDownKeepsItsLastRelayReport(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer)
	f.reconcileProxyGroup(r, "gateway")

	pods := connectProxiesReporting(f, "gateway", agent.VoiceRelayRunning)
	if len(pods) < 2 {
		t.Fatalf("the group has %d proxy pods, want 2", len(pods))
	}
	f.agents.ReportVoiceRelay(string(pods[0].UID), agent.VoiceRelayBindFailed)
	f.agents.Disconnect(string(pods[0].UID))
	f.reconcileProxyGroup(r, "gateway")

	c := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions, spawneryv1alpha1.ConditionVoice)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != spawneryv1alpha1.ReasonVoiceRelayBindFailed {
		t.Fatalf("Voice = %+v, want False/%s: the proxy keeps serving without a bound relay and cannot retry",
			c, spawneryv1alpha1.ReasonVoiceRelayBindFailed)
	}
}

func TestAFailedBindOutranksAProxyThatDoesNotReport(t *testing.T) {
	f := newFixture(t)
	f.enableVoice(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", exposeLoadBalancer, func(g *spawneryv1alpha1.ProxyGroup) { g.Spec.Replicas = 3 })
	f.reconcileProxyGroup(r, "gateway")

	pods := connectProxiesReporting(f, "gateway", agent.VoiceRelayRunning)
	if len(pods) < 3 {
		t.Fatalf("the group has %d proxy pods, want 3", len(pods))
	}
	f.agents.ReportVoiceRelay(string(pods[0].UID), agent.VoiceRelayBindFailed)
	f.agents.ReportVoiceRelay(string(pods[1].UID), agent.VoiceRelayNotReported)
	f.reconcileProxyGroup(r, "gateway")

	c := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions, spawneryv1alpha1.ConditionVoice)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != spawneryv1alpha1.ReasonVoiceRelayBindFailed {
		t.Fatalf("Voice = %+v, want False/%s beside a proxy on an old image", c, spawneryv1alpha1.ReasonVoiceRelayBindFailed)
	}
}
