/*
Copyright The Spawnery Authors.

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
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	// MinecraftPort is the port every Paper server listens on.
	MinecraftPort int32 = 25565
	// MinecraftPortName names that port.
	MinecraftPortName = "minecraft"

	// ContainerName is the name of the Paper container.
	ContainerName = "minecraft"

	// DataVolumeName is the server's working directory: an emptyDir for
	// ephemeral groups, a PVC for persistent ones.
	DataVolumeName = "data"
	// TmpVolumeName is scratch space, needed because the root filesystem is
	// read-only.
	TmpVolumeName = "tmp"

	// DataMountPath is where DataVolumeName is mounted.
	DataMountPath = "/data"
	// TmpMountPath is where TmpVolumeName is mounted.
	TmpMountPath = "/tmp"

	// SLPHealthBinary is the Server-List-Ping tool baked into the base image.
	// Kubelet knows no SLP probe type, and a tcpSocket probe on 25565 turns
	// green before the world is loaded.
	SLPHealthBinary = "/usr/local/bin/spawnery-slp"
)

// DataClaimName is the name of the PVC of a persistent server.
func DataClaimName(server string) string {
	return server + "-" + DataVolumeName
}

// BuildServerPod renders the pod of one Server. The Server owns the pod, so
// deleting the Server cascades.
func BuildServerPod(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
) (*corev1.Pod, error) {
	if group.Spec.Image == "" {
		return nil, fmt.Errorf("server group %q has no image", group.Name)
	}

	resources := group.Spec.Resources
	if resources == nil && net.Spec.Defaults != nil {
		resources = net.Spec.Defaults.Resources
	}

	// A group's scheduling replaces the network default wholesale. Merging the
	// two would make it impossible to drop an inherited nodeSelector.
	scheduling := group.Spec.Scheduling
	if scheduling == nil && net.Spec.Defaults != nil {
		scheduling = net.Spec.Defaults.Scheduling
	}

	var pullSecrets []corev1.LocalObjectReference
	if net.Spec.Defaults != nil {
		pullSecrets = net.Spec.Defaults.ImagePullSecrets
	}

	volumes := []corev1.Volume{
		dataVolume(group, srv),
		{
			Name:         TmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: DataVolumeName, MountPath: DataMountPath},
		{Name: TmpVolumeName, MountPath: TmpMountPath},
	}

	for _, m := range group.Spec.Mounts {
		volumes = append(volumes, corev1.Volume{
			Name: m.Name,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: m.ConfigMap,
				Secret:    m.Secret,
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			ReadOnly:  true,
		})
	}

	container := corev1.Container{
		Name:  ContainerName,
		Image: group.Spec.Image,
		Ports: []corev1.ContainerPort{{
			Name:          MinecraftPortName,
			ContainerPort: MinecraftPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: []corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: "SPAWNERY_SERVER", Value: srv.Name},
			{Name: "SPAWNERY_MAX_PLAYERS", Value: strconv.FormatInt(int64(group.Spec.MaxPlayers), 10)},
		},
		VolumeMounts: mounts,
		// Readiness only. A liveness probe would restart the container and
		// kick every player on it — the state machine handles a red readiness
		// probe by deregistering instead.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						SLPHealthBinary,
						"--host", "127.0.0.1",
						"--port", strconv.FormatInt(int64(MinecraftPort), 10),
					},
				},
			},
			InitialDelaySeconds: 20,
			PeriodSeconds:       5,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if resources != nil {
		container.Resources = *resources
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srv.Name,
			Namespace: srv.Namespace,
			Labels:    ServerLabels(net.Name, group.Name, srv.Name),
			Annotations: map[string]string{
				AnnotationSafeToEvict: "false",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spawneryv1alpha1.GroupVersion.String(),
				Kind:               "Server",
				Name:               srv.Name,
				UID:                srv.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{container},
			Volumes:       volumes,
			RestartPolicy: corev1.RestartPolicyAlways,
			// The pods carry no Kubernetes credentials. Milestone 2 mounts a
			// projected, audience-bound token for the gRPC channel instead.
			AutomountServiceAccountToken:  ptr.To(false),
			ImagePullSecrets:              pullSecrets,
			TerminationGracePeriodSeconds: ptr.To(group.Spec.TerminationGracePeriodSeconds),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		},
	}

	if scheduling != nil {
		pod.Spec.NodeSelector = scheduling.NodeSelector
		pod.Spec.Tolerations = scheduling.Tolerations
		pod.Spec.Affinity = scheduling.Affinity
	}

	return pod, nil
}

func dataVolume(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) corev1.Volume {
	if group.Spec.Type == spawneryv1alpha1.ServerGroupPersistent {
		return corev1.Volume{
			Name: DataVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: DataClaimName(srv.Name),
				},
			},
		}
	}
	return corev1.Volume{
		Name:         DataVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}
