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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// Copies of internal/worldsync's names; its agreement test reads them from
// here, so podspec does not import the S3 client.
const (
	WorldSyncDriver      = "worldsync.spawnery.cloud"
	WorldSyncAttrWorld   = "world"
	WorldSyncAttrKeep    = "keep"
	WorldSyncAttrReplace = "replace"
	EnvWorldSync         = "SPAWNERY_WORLD_SYNC"
	EnvWorldSyncInterval = "SPAWNERY_WORLD_SYNC_INTERVAL"
)

func worldSyncVolume(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) corev1.Volume {
	attrs := map[string]string{
		WorldSyncAttrWorld: srv.Namespace + "/" + group.Name + "/" + srv.Spec.Key,
		WorldSyncAttrKeep:  strings.Join(group.Spec.Storage.Keep, "\n"),
	}
	if len(group.Spec.Storage.Replace) > 0 {
		attrs[WorldSyncAttrReplace] = strings.Join(group.Spec.Storage.Replace, "\n")
	}
	return corev1.Volume{
		Name: DataVolumeName,
		VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver:           WorldSyncDriver,
			VolumeAttributes: attrs,
		}},
	}
}

func worldSyncEnv(group *spawneryv1alpha1.ServerGroup) []corev1.EnvVar {
	if !group.UsesObjectStore() {
		return nil
	}
	return []corev1.EnvVar{{Name: EnvWorldSync, Value: "1"}}
}

// WithWorldSyncInterval runs after BuildServerPod, like WithAOTCache, so
// changing the operator's interval does not restart every world.
func WithWorldSyncInterval(pod *corev1.Pod, d time.Duration) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		for _, e := range c.Env {
			if e.Name == EnvWorldSync {
				c.Env = append(c.Env, corev1.EnvVar{Name: EnvWorldSyncInterval, Value: d.String()})
				break
			}
		}
	}
}
