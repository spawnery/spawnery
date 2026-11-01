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

package netstate

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// ToStartResources is the wire form of a member's stamped values; nil for an
// unstamped member.
func ToStartResources(r *corev1.ResourceRequirements, heap *resource.Quantity) *agentpb.StartResources {
	if r == nil {
		return nil
	}
	out := &agentpb.StartResources{
		MemoryRequest: quantity(r.Requests, corev1.ResourceMemory),
		MemoryLimit:   quantity(r.Limits, corev1.ResourceMemory),
		CpuRequest:    quantity(r.Requests, corev1.ResourceCPU),
		CpuLimit:      quantity(r.Limits, corev1.ResourceCPU),
	}
	if heap != nil {
		out.Heap = heap.String()
	}
	return out
}

func quantity(list corev1.ResourceList, name corev1.ResourceName) string {
	q, ok := list[name]
	if !ok {
		return ""
	}
	return q.String()
}
