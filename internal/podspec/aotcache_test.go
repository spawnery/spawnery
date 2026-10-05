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

package podspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const sha = "0000000000000000000000000000000000000000000000000000000000000000"

func TestAOTCacheImage(t *testing.T) {
	for _, tc := range []struct{ image, want string }{
		{"ghcr.io/spawnery/purpur:26.3-0.23.0", "ghcr.io/spawnery/purpur-aot:26.3-0.23.0"},
		{"ghcr.io/spawnery/purpur:26.2-0.23.0", "ghcr.io/spawnery/purpur-aot:26.2-0.23.0"},
		{"ghcr.io/spawnery/purpur:26.3-1.0.0", "ghcr.io/spawnery/purpur-aot:26.3-1.0.0"},
		{"ghcr.io/spawnery/purpur:26.3-0.24.10", "ghcr.io/spawnery/purpur-aot:26.3-0.24.10"},
		{"ghcr.io/spawnery/purpur:26.3-0.23.0@sha256:" + sha, "ghcr.io/spawnery/purpur-aot:26.3-0.23.0"},
		{"ghcr.io/spawnery/paper:26.3-0.23.0", ""},
		{"ghcr.io/spawnery/purpur:26.3-0.22.0", ""},
		{"ghcr.io/spawnery/paper:1.21.4-0.1.0", ""},
		{"ghcr.io/spawnery/purpur@sha256:" + sha, ""},
		{"ghcr.io/spawnery/purpur", ""},
		{"ghcr.io/spawnery/purpur:latest", ""},
		{"ghcr.io/spawnery/purpur:26.3-0.23.0-rc.1", ""},
		{"ghcr.io/spawnery/purpur:26.3-0.23", ""},
		{"ghcr.io/spawnery/velocity:3.5.1-0.23.0", ""},
		{"ghcr.io/spawnery/purpur-aot:26.3-0.23.0", ""},
		{"registry.example.net/spawnery/purpur:26.3-0.23.0", ""},
		{"localhost:5000/ghcr.io/spawnery/purpur:26.3-0.23.0", ""},
	} {
		if got := AOTCacheImage(tc.image); got != tc.want {
			t.Errorf("AOTCacheImage(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

func TestWithAOTCacheMountsTheCacheImageReadOnly(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Image = "ghcr.io/spawnery/purpur:26.3-0.23.0"
	})
	WithAOTCache(pod)

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == AOTCacheVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Image == nil ||
		vol.Image.Reference != "ghcr.io/spawnery/purpur-aot:26.3-0.23.0" ||
		vol.Image.PullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("cache volume = %+v, want the purpur-aot image, IfNotPresent", vol)
	}
	var mounted bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == AOTCacheVolumeName {
			mounted = m.MountPath == AOTCacheMountPath && m.ReadOnly
		}
	}
	if !mounted {
		t.Errorf("mounts = %+v, want %s read-only at %s",
			pod.Spec.Containers[0].VolumeMounts, AOTCacheVolumeName, AOTCacheMountPath)
	}
}

func TestWithAOTCacheLeavesOtherImagesAlone(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Image = "ghcr.io/spawnery/purpur:26.3-0.22.0"
	})
	volumes, mounts := len(pod.Spec.Volumes), len(pod.Spec.Containers[0].VolumeMounts)
	WithAOTCache(pod)
	if len(pod.Spec.Volumes) != volumes || len(pod.Spec.Containers[0].VolumeMounts) != mounts {
		t.Errorf("an image without a cache got a volume or mount: %+v", pod.Spec.Volumes)
	}
}
