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
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	ProxyContainerName = "velocity"

	// ProxyReadyPort is bound by the Velocity agent only after its first
	// FullSync, so a proxy without a server list cannot turn green and
	// disconnect every player with "no available server".
	ProxyReadyPort     int32 = 8081
	ProxyReadyPortName       = "ready"

	// EnvPlayerLimit is load-bearing: the registry discards any report above it.
	EnvPlayerLimit = "SPAWNERY_PLAYER_LIMIT"
	// EnvFallbackGroups is the same list the operator puts in
	// DrainPlayers.toGroups, comma separated. The agent refuses to connect on an
	// empty value, which the CRD's MinItems=1 makes an operator bug.
	EnvFallbackGroups            = "SPAWNERY_FALLBACK_GROUPS"
	EnvProxy                     = "SPAWNERY_PROXY"
	EnvTransferForceAfterSeconds = "SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS"
	EnvForwardingSecretFile      = "SPAWNERY_FORWARDING_SECRET_FILE"

	// DefaultPlayerLimit exists because zero would make the registry discard
	// every count.
	DefaultPlayerLimit int32 = 500

	// DefaultDrainTimeoutSeconds mirrors the CRD default for objects that never
	// went through API server defaulting; a zero grace period would kill a
	// proxy's sessions the instant it was replaced.
	DefaultDrainTimeoutSeconds int32 = 300
)

// ProxyPlayerLimit feeds both SPAWNERY_PLAYER_LIMIT and velocity.toml's
// show-max-players, which must agree. A zero in spec.config means unset.
func ProxyPlayerLimit(group *spawneryv1alpha1.ProxyGroup) int32 {
	if cfg := group.Spec.Config; cfg != nil && cfg.PlayerLimit > 0 {
		return cfg.PlayerLimit
	}
	return DefaultPlayerLimit
}

// BuildProxyPod's pod is owned by the group: proxies are fungible and have no
// CR of their own.
func BuildProxyPod(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	name string,
	agentEndpoint string,
	configValues []byte,
) (*corev1.Pod, error) {
	pod, err := renderProxyPod(net, group, name, agentEndpoint)
	if err != nil {
		return nil, err
	}

	// Stamped here, not by the caller, so no proxy pod can miss it. configValues
	// is passed through rather than accepted as a digest so a caller cannot
	// stamp a wrong one.
	hash, err := DesiredProxyHash(net, group, agentEndpoint, configValues)
	if err != nil {
		return nil, err
	}
	pod.Labels[LabelPodHash] = hash

	return pod, nil
}

// renderProxyPod is shared with DesiredProxyHash, which passes an empty name
// so nothing derived from it (such as SPAWNERY_PROXY) reaches the digest.
func renderProxyPod(
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

	scheduling := EffectiveScheduling(net, group.Spec.Scheduling)

	var pullSecrets []corev1.LocalObjectReference
	if net.Spec.Defaults != nil {
		pullSecrets = net.Spec.Defaults.ImagePullSecrets
	}

	playerLimit := ProxyPlayerLimit(group)

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
							// The audience makes a standard API server token worthless here.
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
		configVolume(GroupConfigMapName(group.Name, RoleProxy), net.Spec.ForwardingSecretRef.Name),
	}
	mounts := []corev1.VolumeMount{
		{Name: DataVolumeName, MountPath: DataMountPath},
		{Name: TmpVolumeName, MountPath: TmpMountPath},
		{Name: AgentVolumeName, MountPath: AgentMountPath, ReadOnly: true},
		{Name: ConfigVolumeName, MountPath: ConfigMountPath, ReadOnly: true},
	}
	if vol := configOverlayVolume(group.Spec.ConfigOverlay); vol != nil {
		volumes = append(volumes, *vol)
		mounts = append(mounts, corev1.VolumeMount{
			Name:      ConfigOverlayVolumeName,
			MountPath: path.Join(ConfigMountPath, configOverlayDir),
			ReadOnly:  true,
		})
	}

	userVolumes, userVolumeMounts, err := renderUserMounts(group.Spec.Mounts)
	if err != nil {
		return nil, err
	}
	volumes = append(volumes, userVolumes...)
	mounts = append(mounts, userVolumeMounts...)

	// Read-only at the volume as well as the mount: one claim may serve several
	// groups.
	if group.Spec.ExtraPlugins != nil {
		volumes = append(volumes, sourceVolume(PluginSourceVolumeName,
			group.Spec.ExtraPlugins.ClaimName, group.Spec.ExtraPlugins.Image, group.Spec.ExtraPlugins.PullPolicy))
		mounts = append(mounts, corev1.VolumeMount{
			Name:      PluginSourceVolumeName,
			MountPath: PluginSourceMountPath,
			ReadOnly:  true,
		})
	}

	if group.Spec.ExtraFiles != nil {
		volumes = append(volumes, sourceVolume(FileSourceVolumeName,
			group.Spec.ExtraFiles.ClaimName, group.Spec.ExtraFiles.Image, group.Spec.ExtraFiles.PullPolicy))
		mounts = append(mounts, corev1.VolumeMount{
			Name:      FileSourceVolumeName,
			MountPath: FileSourceMountPath,
			ReadOnly:  true,
		})
	}

	minecraft := corev1.ContainerPort{
		Name:          MinecraftPortName,
		ContainerPort: MinecraftPort,
		Protocol:      corev1.ProtocolTCP,
	}
	// HostPort caps replicas at the node count, since the scheduler will not put
	// two pods with the same host port on one node. Applied here so it reaches
	// the hash; the nil check matters for objects that skipped the CRD's CEL
	// rules.
	if group.Spec.Expose.Type == spawneryv1alpha1.ExposeHostPort &&
		group.Spec.Expose.HostPort != nil {
		minecraft.HostPort = group.Spec.Expose.HostPort.Port
	}

	container := corev1.Container{
		Name:  ProxyContainerName,
		Image: group.Spec.Image,
		Stdin: true,

		Ports: []corev1.ContainerPort{
			minecraft,
			{
				Name:          ProxyReadyPortName,
				ContainerPort: ProxyReadyPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: append(append(append([]corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: EnvProxy, Value: name},
			{Name: EnvPlayerLimit, Value: strconv.FormatInt(int64(playerLimit), 10)},
			{Name: EnvFallbackGroups, Value: strings.Join(group.Spec.Routing.FallbackGroups, ",")},
			{Name: EnvOperatorEndpoint, Value: agentEndpoint},
		}, transferEnv(group)...), substitutionEnv(group.Spec.Substitution)...), group.Spec.Env...),
		VolumeMounts: mounts,
		// Readiness only: a restart would disconnect every player on this proxy.
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
			// FSGroup: a writable claim on spec.mounts arrives owned by root.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:        ptr.To(true),
				SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				FSGroup:             ptr.To(FSGroupID),
				FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
			},
		},
	}

	if scheduling != nil {
		pod.Spec.NodeSelector = scheduling.NodeSelector
		pod.Spec.Tolerations = scheduling.Tolerations
		pod.Spec.Affinity = scheduling.Affinity
	}

	// Stamped from the Network's status so the Secret has one reader. An absent
	// label means "unknown", see LabelForwardingHash.
	if hash := net.Status.ForwardingSecretHash; hash != "" {
		pod.Labels[LabelForwardingHash] = hash
	}

	return pod, nil
}

func transferEnv(group *spawneryv1alpha1.ProxyGroup) []corev1.EnvVar {
	after, ok := group.TransferForceAfter()
	if !ok {
		return nil
	}
	return []corev1.EnvVar{
		{Name: EnvTransferForceAfterSeconds, Value: strconv.FormatInt(int64(after/time.Second), 10)},
		{Name: EnvForwardingSecretFile, Value: path.Join(ConfigMountPath, configSecretFile)},
	}
}
