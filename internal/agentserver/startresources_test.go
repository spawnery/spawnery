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

package agentserver

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
)

func q(s string) resource.Quantity { return resource.MustParse(s) }

func onDemandGroup(bounds corev1.ResourceList, resources *corev1.ResourceRequirements) *spawneryv1alpha1.ServerGroup {
	g := &spawneryv1alpha1.ServerGroup{}
	g.Name = "dungeon"
	g.Spec.Type = spawneryv1alpha1.ServerGroupOnDemand
	g.Spec.Resources = resources
	if bounds != nil {
		g.Spec.MemberResources = &spawneryv1alpha1.MemberResourcesSpec{Max: bounds}
	}
	return g
}

func standardMax() corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: q("16Gi"), corev1.ResourceCPU: q("8")}
}

func standardBase() *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: q("2Gi"), corev1.ResourceCPU: q("1")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: q("4Gi"), corev1.ResourceCPU: q("2")},
	}
}

func resolve(t *testing.T, g *spawneryv1alpha1.ServerGroup, req *agentpb.StartResources) Resolved {
	t.Helper()
	res, stamp, err := ResolveStartResources(g, g.Spec.Resources, req)
	if err != nil {
		t.Fatalf("resolve %v: %v", req, err)
	}
	if !stamp {
		t.Fatalf("resolve %v: stamp is false on a group with memberResources", req)
	}
	return res
}

func wantQuantity(t *testing.T, what string, list corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	got, ok := list[name]
	if !ok {
		t.Fatalf("%s %s: absent, want %s", what, name, want)
	}
	if got.Cmp(q(want)) != 0 {
		t.Fatalf("%s %s: %s, want %s", what, name, got.String(), want)
	}
}

func wantHeap(t *testing.T, res Resolved, want string) {
	t.Helper()
	if res.Heap == nil {
		t.Fatalf("heap: nil, want %s", want)
	}
	if res.Heap.Cmp(q(want)) != 0 {
		t.Fatalf("heap: %s, want %s", res.Heap.String(), want)
	}
}

func TestResolveWithoutMemberResources(t *testing.T) {
	g := onDemandGroup(nil, standardBase())

	res, stamp, err := ResolveStartResources(g, g.Spec.Resources, nil)
	if err != nil || stamp || res.Heap != nil {
		t.Fatalf("no values: res=%+v stamp=%v err=%v, want nothing", res, stamp, err)
	}

	_, stamp, err = ResolveStartResources(g, g.Spec.Resources, &agentpb.StartResources{})
	if err != nil || stamp {
		t.Fatalf("all-empty values: stamp=%v err=%v, want neither", stamp, err)
	}

	_, _, err = ResolveStartResources(g, g.Spec.Resources, &agentpb.StartResources{MemoryLimit: "4Gi"})
	if !errors.Is(err, ErrNoMemberResources) {
		t.Fatalf("values on a group without memberResources: %v, want ErrNoMemberResources", err)
	}
}

func TestResolveFillsEmptyFieldsFromTheGroup(t *testing.T) {
	g := onDemandGroup(standardMax(), standardBase())

	res := resolve(t, g, &agentpb.StartResources{MemoryLimit: "7Gi"})

	wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceMemory, "7Gi")
	wantQuantity(t, "requests", res.Resources.Requests, corev1.ResourceMemory, "2Gi")
	wantQuantity(t, "requests", res.Resources.Requests, corev1.ResourceCPU, "1")
	wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceCPU, "2")
	wantHeap(t, res, "5Gi")

	if g.Spec.Resources.Limits.Memory().Cmp(q("4Gi")) != 0 {
		t.Fatalf("the group's own resources were changed: %v", g.Spec.Resources)
	}
}

func TestResolveLowersTheRequestToANewSmallerLimit(t *testing.T) {
	base := standardBase()
	base.Requests[corev1.ResourceMemory] = q("4Gi")
	g := onDemandGroup(standardMax(), base)

	res := resolve(t, g, &agentpb.StartResources{MemoryLimit: "3Gi"})

	wantQuantity(t, "requests", res.Resources.Requests, corev1.ResourceMemory, "3Gi")
	wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceMemory, "3Gi")
	wantHeap(t, res, "1Gi")
}

func TestResolveTakesNetworkDefaultsAndRequestEqualsLimit(t *testing.T) {
	net := &spawneryv1alpha1.Network{}
	net.Spec.Defaults = &spawneryv1alpha1.Defaults{Resources: &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: q("4Gi")},
	}}
	g := onDemandGroup(standardMax(), nil)

	base := EffectiveResources(net, g)
	if base == nil || base.Limits.Memory().Cmp(q("4Gi")) != 0 {
		t.Fatalf("EffectiveResources: %v, want the network default", base)
	}

	for _, tc := range []struct{ limit, request string }{
		{limit: "8Gi", request: "4Gi"},
		{limit: "3Gi", request: "3Gi"},
	} {
		res, stamp, err := ResolveStartResources(g, base, &agentpb.StartResources{MemoryLimit: tc.limit})
		if err != nil || !stamp {
			t.Fatalf("limit %s: stamp=%v err=%v", tc.limit, stamp, err)
		}
		wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceMemory, tc.limit)
		wantQuantity(t, "requests", res.Resources.Requests, corev1.ResourceMemory, tc.request)
		if _, ok := res.Resources.Requests[corev1.ResourceCPU]; ok {
			t.Fatalf("limit %s: a cpu request appeared from nowhere: %v", tc.limit, res.Resources)
		}
		if _, ok := res.Resources.Limits[corev1.ResourceCPU]; ok {
			t.Fatalf("limit %s: a cpu limit appeared from nowhere: %v", tc.limit, res.Resources)
		}
	}
}

func TestEffectiveResourcesPrefersTheGroupWhole(t *testing.T) {
	net := &spawneryv1alpha1.Network{}
	net.Spec.Defaults = &spawneryv1alpha1.Defaults{Resources: &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: q("4Gi"), corev1.ResourceCPU: q("2")},
	}}
	own := &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("6Gi")}}
	g := onDemandGroup(standardMax(), own)

	if got := EffectiveResources(net, g); !equality.Semantic.DeepEqual(got, own) {
		t.Fatalf("EffectiveResources: %v, want the group's own resources with nothing merged in", got)
	}
	if got := EffectiveResources(&spawneryv1alpha1.Network{}, onDemandGroup(standardMax(), nil)); got != nil {
		t.Fatalf("EffectiveResources without group or network values: %v, want nil", got)
	}
}

func TestResolveUsesAGivenHeap(t *testing.T) {
	g := onDemandGroup(standardMax(), standardBase())

	res := resolve(t, g, &agentpb.StartResources{MemoryLimit: "8Gi", Heap: "5500Mi"})

	wantHeap(t, res, "5500Mi")
	wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceMemory, "8Gi")
}

func TestResolveDerivesTheHeapWithTheGroupsNonHeap(t *testing.T) {
	g := onDemandGroup(standardMax(), standardBase())
	nonHeap := q("3Gi")
	g.Spec.MemberResources.NonHeap = &nonHeap

	wantHeap(t, resolve(t, g, &agentpb.StartResources{MemoryLimit: "8Gi"}), "5Gi")
}

func TestResolveStampsAStartWithoutValues(t *testing.T) {
	g := onDemandGroup(standardMax(), standardBase())

	for _, req := range []*agentpb.StartResources{nil, {}} {
		res := resolve(t, g, req)
		if !equality.Semantic.DeepEqual(res.Resources, *standardBase()) {
			t.Fatalf("resources %v, want the group's %v", res.Resources, *standardBase())
		}
		if res.Heap != nil {
			t.Fatalf("heap %s, want none", res.Heap.String())
		}
	}
}

func wantRefusal(t *testing.T, err error, field string, numbers ...string) {
	t.Helper()
	var refusal *ResourceRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err %v, want a *ResourceRefusal", err)
	}
	if refusal.Field != field {
		t.Fatalf("refusal field %q (%s), want %q", refusal.Field, refusal.Message, field)
	}
	for _, n := range numbers {
		if !strings.Contains(refusal.Message, n) {
			t.Fatalf("refusal message %q does not name %s", refusal.Message, n)
		}
	}
}

func TestResolveRefuses(t *testing.T) {
	memoryOnlyMax := corev1.ResourceList{corev1.ResourceMemory: q("16Gi")}
	noMemoryLimit := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: q("2Gi")},
	}
	for _, tc := range []struct {
		name    string
		max     corev1.ResourceList
		base    *corev1.ResourceRequirements
		req     *agentpb.StartResources
		field   string
		numbers []string
	}{
		{"request above limit", standardMax(), standardBase(),
			&agentpb.StartResources{MemoryRequest: "8Gi", MemoryLimit: "6Gi"}, "memory", []string{"8Gi", "6Gi"}},
		{"request above the group's limit", standardMax(), standardBase(),
			&agentpb.StartResources{CpuRequest: "3"}, "cpu", []string{"3", "2"}},
		{"heap not below limit", standardMax(), standardBase(),
			&agentpb.StartResources{MemoryLimit: "6Gi", Heap: "7Gi"}, "heap", []string{"7Gi", "6Gi"}},
		{"heap equal to the group's limit", standardMax(), standardBase(),
			&agentpb.StartResources{Heap: "4Gi"}, "heap", []string{"4Gi"}},
		{"heap without any memory limit", standardMax(), noMemoryLimit,
			&agentpb.StartResources{Heap: "3Gi"}, "heap", []string{"3Gi"}},
		{"limit above max", standardMax(), standardBase(),
			&agentpb.StartResources{MemoryLimit: "20Gi"}, "memory", []string{"20Gi", "16Gi"}},
		{"cpu above max", standardMax(), standardBase(),
			&agentpb.StartResources{CpuLimit: "12"}, "cpu", []string{"12", "8"}},
		{"cpu while max names no cpu", memoryOnlyMax, standardBase(),
			&agentpb.StartResources{CpuLimit: "2"}, "cpu", []string{"2"}},
		{"a limit leaving no heap", standardMax(), standardBase(),
			&agentpb.StartResources{MemoryLimit: "1Gi"}, "memory", []string{"1Gi", "2Gi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := onDemandGroup(tc.max, tc.base)
			_, _, err := ResolveStartResources(g, g.Spec.Resources, tc.req)
			wantRefusal(t, err, tc.field, tc.numbers...)
		})
	}
}

func TestResolveChecksGroupValuesAgainstMax(t *testing.T) {
	base := standardBase()
	base.Limits[corev1.ResourceMemory] = q("20Gi")
	g := onDemandGroup(standardMax(), base)

	_, _, err := ResolveStartResources(g, g.Spec.Resources, &agentpb.StartResources{CpuLimit: "2"})
	wantRefusal(t, err, "memory", "20Gi", "16Gi")

	_, _, err = ResolveStartResources(g, g.Spec.Resources, nil)
	wantRefusal(t, err, "memory", "20Gi", "16Gi")
}

func TestResolveParsesQuantities(t *testing.T) {
	g := onDemandGroup(standardMax(), standardBase())

	res := resolve(t, g, &agentpb.StartResources{MemoryLimit: "7G"})
	wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceMemory, "7G")

	res = resolve(t, g, &agentpb.StartResources{MemoryLimit: "7168Mi"})
	wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceMemory, "7Gi")
	wantHeap(t, res, "5Gi")

	res = resolve(t, g, &agentpb.StartResources{CpuLimit: "0.5"})
	wantQuantity(t, "limits", res.Resources.Limits, corev1.ResourceCPU, "500m")
	wantQuantity(t, "requests", res.Resources.Requests, corev1.ResourceCPU, "500m")

	for _, tc := range []struct {
		req   *agentpb.StartResources
		field string
		value string
	}{
		{&agentpb.StartResources{MemoryLimit: "7 Gi"}, "memory", "7 Gi"},
		{&agentpb.StartResources{MemoryRequest: "-1Gi"}, "memory", "-1Gi"},
		{&agentpb.StartResources{CpuLimit: "0"}, "cpu", "0"},
		{&agentpb.StartResources{CpuRequest: "lots"}, "cpu", "lots"},
		{&agentpb.StartResources{MemoryLimit: "8Gi", Heap: "0"}, "heap", "0"},
	} {
		_, _, err := ResolveStartResources(g, g.Spec.Resources, tc.req)
		wantRefusal(t, err, tc.field, tc.value)
	}
}
