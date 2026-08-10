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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	// ProxyContainerName is the name of the Velocity container.
	ProxyContainerName = "velocity"

	// ProxyReadyPort is the ready gate of design 6.6. The Velocity agent binds
	// it only once it has processed its first FullSync, so a proxy cannot turn
	// green before it has a server list — a plain tcpSocket check on 25565
	// would, and a proxy that gets traffic without a list disconnects every
	// player with "no available server".
	ProxyReadyPort int32 = 8081
	// ProxyReadyPortName names that port.
	ProxyReadyPortName = "ready"

	// EnvPlayerLimit names the container env var carrying the proxy's player
	// limit. The agent reports it as slots, and the registry discards any
	// report above it, so it is load-bearing rather than cosmetic.
	EnvPlayerLimit = "SPAWNERY_PLAYER_LIMIT"
	// EnvProxy names the container env var carrying the pod's own name.
	EnvProxy = "SPAWNERY_PROXY"

	// DefaultPlayerLimit is what a ProxyGroup that sets none gets. Zero would
	// be worse than a guess: the registry rejects every report where players
	// exceed slots, so a limit of zero would silently discard every count.
	DefaultPlayerLimit int32 = 500

	// DefaultDrainTimeoutSeconds mirrors the CRD default on
	// ProxyGroup.spec.drain. It is repeated here because a ProxyGroup built in
	// a unit test never passes through the API server's defaulting, and a nil
	// drain block must not produce a grace period of zero — that would kill a
	// proxy's sessions the instant it was replaced.
	DefaultDrainTimeoutSeconds int32 = 300
)

// BuildProxyPod renders one pod of a ProxyGroup. The group owns the pod, so
// deleting the group cascades — there is no per-proxy CR to hang it from, and
// none is wanted: proxies are fungible and have no state machine.
func BuildProxyPod(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	name string,
	agentEndpoint string,
) (*corev1.Pod, error) {
	if group.Spec.Image == "" {
		return nil, fmt.Errorf("proxy group %q has no image", group.Name)
	}
	if agentEndpoint == "" {
		return nil, fmt.Errorf("proxy group %q has no agent endpoint", group.Name)
	}

	resources := group.Spec.Resources
	if resources == nil && net.Spec.Defaults != nil {
		resources = net.Spec.Defaults.Resources
	}

	// A group's scheduling replaces the network default wholesale, exactly as
	// it does for server groups: merging the two would make it impossible to
	// drop an inherited nodeSelector.
	scheduling := group.Spec.Scheduling
	if scheduling == nil && net.Spec.Defaults != nil {
		scheduling = net.Spec.Defaults.Scheduling
	}

	var pullSecrets []corev1.LocalObjectReference
	if net.Spec.Defaults != nil {
		pullSecrets = net.Spec.Defaults.ImagePullSecrets
	}

	playerLimit := DefaultPlayerLimit
	if group.Spec.Config != nil && group.Spec.Config.PlayerLimit > 0 {
		playerLimit = group.Spec.Config.PlayerLimit
	}

	grace := DefaultDrainTimeoutSeconds
	if group.Spec.Drain != nil {
		grace = group.Spec.Drain.TimeoutSeconds
	}

	volumes := []corev1.Volume{
		{
			Name:         DataVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         TmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: AgentVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							// The audience is what makes a standard API server
							// token worthless here, and the short expiry keeps
							// the replay window small. The kubelet rotates it.
							ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Audience:          AgentTokenAudience,
								ExpirationSeconds: ptr.To(TokenExpirationSeconds),
								Path:              AgentTokenPath,
							},
						},
						{
							ConfigMap: &corev1.ConfigMapProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: CAConfigMapName},
								Items: []corev1.KeyToPath{
									{Key: CAConfigMapKey, Path: AgentCAPath},
								},
							},
						},
					},
				},
			},
		},
	}

	container := corev1.Container{
		Name:  ProxyContainerName,
		Image: group.Spec.Image,
		Ports: []corev1.ContainerPort{
			{
				Name:          MinecraftPortName,
				ContainerPort: MinecraftPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          ProxyReadyPortName,
				ContainerPort: ProxyReadyPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: []corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: EnvProxy, Value: name},
			{Name: EnvPlayerLimit, Value: strconv.FormatInt(int64(playerLimit), 10)},
			{Name: EnvOperatorEndpoint, Value: agentEndpoint},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: DataVolumeName, MountPath: DataMountPath},
			{Name: TmpVolumeName, MountPath: TmpMountPath},
			{Name: AgentVolumeName, MountPath: AgentMountPath, ReadOnly: true},
		},
		// Readiness only, for the same reason the server pod has no liveness
		// probe: a restart would disconnect every player on this proxy, and
		// the client connection terminates here.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(ProxyReadyPort),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       5,
			TimeoutSeconds:      3,
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
			Name:      name,
			Namespace: group.Namespace,
			Labels:    ProxyLabels(net.Name, group.Name),
			Annotations: map[string]string{
				AnnotationSafeToEvict: "false",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spawneryv1alpha1.GroupVersion.String(),
				Kind:               "ProxyGroup",
				Name:               group.Name,
				UID:                group.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{container},
			Volumes:                       volumes,
			RestartPolicy:                 corev1.RestartPolicyAlways,
			ServiceAccountName:            ProxyServiceAccountName,
			AutomountServiceAccountToken:  ptr.To(false),
			ImagePullSecrets:              pullSecrets,
			TerminationGracePeriodSeconds: ptr.To(int64(grace)),
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
