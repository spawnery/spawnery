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

// Package netstatus answers /cloud status: what a network's pods use against
// what they asked for, and how its servers keep up with the tick rate.
package netstatus

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"
)

type Usage struct {
	CPUMilli    int64
	MemoryBytes int64
}

// MetricsReader: a pod without a sample is absent, which is not zero.
type MetricsReader interface {
	PodUsage(ctx context.Context, namespace string) (map[string]Usage, error)
}

// APIMetrics uses a plain GET rather than k8s.io/metrics, a new module for
// one list call. The manager's cache cannot watch this list.
type APIMetrics struct {
	REST rest.Interface
}

type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Containers []struct {
			Usage corev1.ResourceList `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=list

func (m APIMetrics) PodUsage(ctx context.Context, namespace string) (map[string]Usage, error) {
	raw, err := m.REST.Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", namespace, "pods").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pod metrics in %s: %w", namespace, err)
	}
	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode pod metrics in %s: %w", namespace, err)
	}
	out := make(map[string]Usage, len(list.Items))
	for _, item := range list.Items {
		var cpu, mem resource.Quantity
		for _, c := range item.Containers {
			if q, ok := c.Usage[corev1.ResourceCPU]; ok {
				cpu.Add(q)
			}
			if q, ok := c.Usage[corev1.ResourceMemory]; ok {
				mem.Add(q)
			}
		}
		out[item.Metadata.Name] = Usage{CPUMilli: cpu.MilliValue(), MemoryBytes: mem.Value()}
	}
	return out, nil
}
