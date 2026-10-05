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

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const EnvSubstitutionPrefix = "SPAWNERY_SUBSTITUTION_PREFIX"

// EnvKeep carries spec.storage.keep to the entrypoint, one entry per line.
const EnvKeep = "SPAWNERY_KEEP"

// EnvReplace carries spec.storage.replace to the entrypoint, one entry per line.
const EnvReplace = "SPAWNERY_REPLACE"

// sourceVolume renders a read-only claim, or an image volume for an image source.
func sourceVolume(name, claim, image string, pull corev1.PullPolicy) corev1.Volume {
	if image != "" {
		if pull == "" {
			pull = corev1.PullIfNotPresent
		}
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{Reference: image, PullPolicy: pull},
		}}
	}
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: true},
	}}
}

func substitutionEnv(s *spawneryv1alpha1.Substitution) []corev1.EnvVar {
	if s == nil {
		return nil
	}
	return []corev1.EnvVar{{Name: EnvSubstitutionPrefix, Value: s.Prefix}}
}

func keepEnv(s *spawneryv1alpha1.StorageSpec) []corev1.EnvVar {
	if s == nil || len(s.Keep) == 0 {
		return nil
	}
	return []corev1.EnvVar{{Name: EnvKeep, Value: strings.Join(s.Keep, "\n")}}
}

func replaceEnv(s *spawneryv1alpha1.StorageSpec) []corev1.EnvVar {
	if s == nil || len(s.Replace) == 0 {
		return nil
	}
	return []corev1.EnvVar{{Name: EnvReplace, Value: strings.Join(s.Replace, "\n")}}
}
