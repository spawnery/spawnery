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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	AOTCacheVolumeName = "aot-cache"
	// AOTCacheMountPath is where image/entrypoint.sh looks for server.aot.
	AOTCacheMountPath = "/var/run/spawnery/aot"

	aotCacheRepository = "ghcr.io/spawnery/purpur"
)

// aotCacheSince is the first image version released with a cache image beside
// each Purpur image; an older tag has none to pull.
var aotCacheSince = [3]int{0, 23, 0}

// AOTCacheImage names the startup cache published with spawnery's Purpur
// image, or returns "" for any image that has none. A digest is dropped and
// the tag kept: the release publishes the cache by tag.
func AOTCacheImage(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || colon < strings.LastIndex(ref, "/") {
		return ""
	}
	repository, tag := ref[:colon], ref[colon+1:]
	if repository != aotCacheRepository {
		return ""
	}
	_, version, ok := strings.Cut(tag, "-")
	if !ok || !atLeast(version, aotCacheSince) {
		return ""
	}
	return repository + "-aot:" + tag
}

func atLeast(version string, floor [3]int) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	var got [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return false
		}
		got[i] = n
	}
	for i := range got {
		if got[i] != floor[i] {
			return got[i] > floor[i]
		}
	}
	return true
}

// WithAOTCache mounts the startup cache of the pod's game image, if it has one.
func WithAOTCache(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != ContainerName {
			continue
		}
		cache := AOTCacheImage(c.Image)
		if cache == "" {
			return
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: AOTCacheVolumeName,
			VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{Reference: cache, PullPolicy: corev1.PullIfNotPresent},
			},
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      AOTCacheVolumeName,
			MountPath: AOTCacheMountPath,
			ReadOnly:  true,
		})
		return
	}
}
