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
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
)

var ErrNoMemberResources = errors.New("that group sets no spec.memberResources, so a start cannot choose its own resources")

// defaultNonHeap is the CRD default, for a group that was never through the apiserver's defaulting.
var defaultNonHeap = resource.MustParse("2Gi")

// minHeap: the pod gets the heap in whole MiB rounded down, and -Xmx0m starts no JVM.
var minHeap = resource.MustParse("1Mi")

// ResourceRefusal is a start's values refused against its group. Field is
// memory, cpu or heap; Message names the value and what it was held against.
type ResourceRefusal struct {
	Field   string
	Message string
}

func (r *ResourceRefusal) Error() string { return r.Message }

func refuseResources(field, format string, args ...any) error {
	return &ResourceRefusal{Field: field, Message: fmt.Sprintf(format, args...)}
}

type Resolved struct {
	Resources corev1.ResourceRequirements
	Heap      *resource.Quantity
}

// EffectiveResources is what BuildServerPod gives a member without values of its own.
func EffectiveResources(net *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) *corev1.ResourceRequirements {
	if g.Spec.Resources != nil {
		return g.Spec.Resources
	}
	if net != nil && net.Spec.Defaults != nil {
		return net.Spec.Defaults.Resources
	}
	return nil
}

func hasStartValues(req *agentpb.StartResources) bool {
	return req.GetMemoryRequest() != "" || req.GetMemoryLimit() != "" ||
		req.GetCpuRequest() != "" || req.GetCpuLimit() != "" || req.GetHeap() != ""
}

// ResolveStartResources fills a start's empty fields from base and checks the
// result against the group's spec.memberResources. stamp is false only for a
// group without memberResources, whose members keep today's pods.
func ResolveStartResources(
	g *spawneryv1alpha1.ServerGroup,
	base *corev1.ResourceRequirements,
	req *agentpb.StartResources,
) (res Resolved, stamp bool, err error) {
	members := g.Spec.MemberResources
	if members == nil {
		if hasStartValues(req) {
			return Resolved{}, false, ErrNoMemberResources
		}
		return Resolved{}, false, nil
	}

	memory, err := parsePair(corev1.ResourceMemory, req.GetMemoryRequest(), req.GetMemoryLimit())
	if err != nil {
		return Resolved{}, false, err
	}
	cpu, err := parsePair(corev1.ResourceCPU, req.GetCpuRequest(), req.GetCpuLimit())
	if err != nil {
		return Resolved{}, false, err
	}
	heap, err := parseStartQuantity("heap", "heap", req.GetHeap())
	if err != nil {
		return Resolved{}, false, err
	}

	var out corev1.ResourceRequirements
	if base != nil {
		base.DeepCopyInto(&out)
	}
	given := map[corev1.ResourceName]startPair{}
	for _, p := range []startPair{memory, cpu} {
		if err := p.resolveInto(&out, members.Max); err != nil {
			return Resolved{}, false, err
		}
		given[p.resource] = p
	}

	memLimit, hasMemLimit := out.Limits[corev1.ResourceMemory]
	if heap == nil && memory.limit != nil {
		nonHeap := defaultNonHeap
		if members.NonHeap != nil {
			nonHeap = *members.NonHeap
		}
		derived := memory.limit.DeepCopy()
		derived.Sub(nonHeap)
		if derived.Cmp(minHeap) < 0 {
			return Resolved{}, false, refuseResources("heap",
				"memory limit %s minus spec.memberResources.nonHeap %s leaves a heap of %s, below %s",
				memory.limit.String(), nonHeap.String(), derived.String(), minHeap.String())
		}
		heap = &derived
	}
	if heap != nil {
		if heap.Cmp(minHeap) < 0 {
			return Resolved{}, false, refuseResources("heap", "heap %s is below %s", heap.String(), minHeap.String())
		}
		if !hasMemLimit {
			return Resolved{}, false, refuseResources("heap",
				"heap %s is given, but neither the start nor the group sets a memory limit", heap.String())
		}
		if heap.Cmp(memLimit) >= 0 {
			return Resolved{}, false, refuseResources("heap",
				"heap %s is not below the memory limit %s%s", heap.String(), memLimit.String(), origin(memory.limit))
		}
	}

	if err := checkMax(out, members.Max, given); err != nil {
		return Resolved{}, false, err
	}
	return Resolved{Resources: out, Heap: heap}, true, nil
}

type startPair struct {
	resource       corev1.ResourceName
	request, limit *resource.Quantity
}

func parsePair(name corev1.ResourceName, request, limit string) (startPair, error) {
	p := startPair{resource: name}
	var err error
	if p.request, err = parseStartQuantity(string(name), string(name)+" request", request); err != nil {
		return p, err
	}
	if p.limit, err = parseStartQuantity(string(name), string(name)+" limit", limit); err != nil {
		return p, err
	}
	return p, nil
}

func parseStartQuantity(field, what, value string) (*resource.Quantity, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, refuseResources(field, "%s %q is not a quantity", what, value)
	}
	if parsed.Sign() <= 0 {
		return nil, refuseResources(field, "%s %s is not positive", what, value)
	}
	return &parsed, nil
}

// resolveInto writes the request out explicitly whenever there is a limit,
// so that neither Kubernetes nor a LimitRange raises it to a new limit. An
// unwritten base request counts as the base limit, or else as the new one.
func (p startPair) resolveInto(out *corev1.ResourceRequirements, bounds corev1.ResourceList) error {
	name := string(p.resource)
	if _, bounded := bounds[p.resource]; !bounded {
		for _, set := range []struct {
			kind string
			q    *resource.Quantity
		}{{"request", p.request}, {"limit", p.limit}} {
			if set.q != nil {
				return refuseResources(name, "%s %s %s is set, but spec.memberResources.max names no %s",
					name, set.kind, set.q.String(), name)
			}
		}
	}

	limit, hasLimit := out.Limits[p.resource]
	if p.limit != nil {
		limit, hasLimit = p.limit.DeepCopy(), true
	}

	request, hasRequest := out.Requests[p.resource]
	if !hasRequest {
		request, hasRequest = out.Limits[p.resource]
	}
	if !hasRequest && hasLimit {
		request, hasRequest = limit.DeepCopy(), true
	}
	switch {
	case p.request != nil:
		request, hasRequest = p.request.DeepCopy(), true
		if hasLimit && request.Cmp(limit) > 0 {
			return refuseResources(name, "%s request %s is above the %s limit %s%s",
				name, request.String(), name, limit.String(), origin(p.limit))
		}
	case hasRequest && hasLimit && request.Cmp(limit) > 0:
		request = limit.DeepCopy()
	}

	if hasLimit {
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		out.Limits[p.resource] = limit
	}
	if hasRequest {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[p.resource] = request.DeepCopy()
	}
	return nil
}

func checkMax(out corev1.ResourceRequirements, bounds corev1.ResourceList, given map[corev1.ResourceName]startPair) error {
	names := make([]corev1.ResourceName, 0, len(bounds))
	for name := range bounds {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	for _, name := range names {
		bound := bounds[name]
		p := given[name]
		for _, v := range []struct {
			kind  string
			list  corev1.ResourceList
			start *resource.Quantity
		}{{"limit", out.Limits, p.limit}, {"request", out.Requests, p.request}} {
			if q, ok := v.list[name]; ok && q.Cmp(bound) > 0 {
				return refuseResources(string(name), "%s %s %s%s is above spec.memberResources.max.%s %s",
					name, v.kind, q.String(), origin(v.start), name, bound.String())
			}
		}
	}
	return nil
}

func origin(fromStart *resource.Quantity) string {
	if fromStart != nil {
		return ""
	}
	return " (the group's)"
}
