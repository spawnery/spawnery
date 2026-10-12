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
		metav1.ConditionTrue, spawneryv1alpha1.ReasonVoiceRelaying) {
		t.Errorf("conditions = %+v, want Voice=True/Relaying", group.Status.Conditions)
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
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30782},
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
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30782},
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
