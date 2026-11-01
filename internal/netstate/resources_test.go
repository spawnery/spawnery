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

package netstate_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/spawnery/spawnery/internal/netstate"
)

func TestToStartResourcesWritesEachQuantity(t *testing.T) {
	heap := resource.MustParse("5Gi")
	got := netstate.ToStartResources(&corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("6Gi"),
			corev1.ResourceCPU:    resource.MustParse("500m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("7Gi"),
			corev1.ResourceCPU:    resource.MustParse("2"),
		},
	}, &heap)
	if got.GetMemoryRequest() != "6Gi" || got.GetMemoryLimit() != "7Gi" ||
		got.GetCpuRequest() != "500m" || got.GetCpuLimit() != "2" || got.GetHeap() != "5Gi" {
		t.Errorf("got %+v", got)
	}
}

func TestToStartResourcesLeavesWhatIsUnsetEmpty(t *testing.T) {
	got := netstate.ToStartResources(&corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("7Gi")},
	}, nil)
	if got.GetMemoryLimit() != "7Gi" || got.GetMemoryRequest() != "" || got.GetCpuRequest() != "" ||
		got.GetCpuLimit() != "" || got.GetHeap() != "" {
		t.Errorf("got %+v", got)
	}
	if netstate.ToStartResources(nil, nil) != nil {
		t.Error("an unstamped member answered with resources")
	}
}
