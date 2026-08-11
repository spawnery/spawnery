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
	"path"
	"strconv"
	"strings"

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

	// AgentVolumeName is the projected volume carrying the agent's token and
	// the CA it verifies the operator's gRPC endpoint with.
	AgentVolumeName = "spawnery-agent"
	// AgentMountPath is where AgentVolumeName is mounted.
	AgentMountPath = "/var/run/spawnery"
	// AgentTokenPath is the projected file holding the audience-bound
	// ServiceAccount token, relative to AgentMountPath.
	AgentTokenPath = "token"
	// AgentCAPath is the projected file holding the operator's CA
	// certificate, relative to AgentMountPath.
	AgentCAPath = "ca.crt"

	// ConfigVolumeName is the projected volume carrying the operator's
	// rendered configuration: the group's own ConfigMap and the Network's
	// forwarding secret. internal/render.Load reads exactly this layout by
	// default, and it is shared verbatim by BuildServerPod and BuildProxyPod
	// through configVolume below, so the two layers cannot drift into
	// different answers about where configuration lives.
	ConfigVolumeName = "spawnery-config"
	// ConfigOverlayVolumeName carries the user's spec.configOverlay
	// ConfigMap, mounted only when a group declares one, nested inside
	// ConfigMountPath at configOverlayDir.
	//
	// It is a plain ConfigMap volume, not a source folded into
	// ConfigVolumeName's own Projected volume, and that is not
	// interchangeable with the alternative: a Projected ConfigMap source
	// only ever surfaces the keys explicitly named in its Items, so the only
	// way to fold an arbitrarily-named overlay key in without enumerating a
	// fixed list — which internal/render's checkOverlayFiles must see even
	// the *wrong* names to refuse loudly, per its own doc comment — would be
	// to guess the flavour's target names ahead of time and hardcode them
	// here. A typo or a name from a different flavour would then be dropped
	// by the kubelet before internal/render ever saw it: no refusal, no
	// crash loop, just an overlay that silently did nothing — the one
	// failure mode this whole area of the design exists to prevent. A plain
	// ConfigMap volume with no Items mounts every key under it unfiltered,
	// so whatever the user actually wrote reaches the renderer and
	// checkOverlayFiles is what decides whether it is accepted.
	ConfigOverlayVolumeName = "spawnery-config-overlay"
	// ConfigMountPath is where ConfigVolumeName is mounted.
	//
	// Not /data/config: Paper writes paper-global.yml and
	// paper-world-defaults.yml there itself at startup, and a ConfigMap
	// mount is always read-only, so a mount there breaks the start —
	// known-issues.md has recorded that collision since milestone 2b.
	// Mounting at ConfigMountPath instead means the collision never arises
	// rather than getting resolved.
	//
	// Not under AgentMountPath: that is the agent's credential mount, and
	// checkMountCollision guards it with a bidirectional nesting check it
	// applies to nothing else. Keeping the two apart keeps that rule saying
	// the one thing it exists to say — ConfigMountPath gets the same
	// bidirectional check below, for the same reason: a user mount there
	// would shadow the file the renderer reads the forwarding secret from.
	ConfigMountPath = "/etc/spawnery"
	// ConfigValuesKey is both the data key of the group's rendered ConfigMap
	// — the key Task 10's controller marshals render.Values into — and the
	// file name it lands at under ConfigMountPath, since that key already
	// matches internal/render.ValuesFile and needs no renaming between the
	// two.
	ConfigValuesKey = "config.yaml"
	// ForwardingSecretKey is the data key of the Network's forwarding
	// Secret, per NetworkSpec.ForwardingSecretRef's documented contract.
	ForwardingSecretKey = "secret"
	// configSecretFile is where ForwardingSecretKey lands under
	// ConfigMountPath. internal/render.SecretFile names the same file
	// independently: podspec stays free of internal/render so that building
	// a pod spec never depends on a package that touches the filesystem.
	configSecretFile = "forwarding.secret"
	// configOverlayDir is the subdirectory the overlay's files land under.
	// internal/render.OverlayDir names the same directory independently, for
	// the reason above — and load.go's own comment on why that loader
	// resolves each entry with os.Stat, rather than trusting DirEntry's
	// Lstat-based type, is exactly why this must be a real subdirectory a
	// ConfigMap is mounted at, not a naming convention layered onto the
	// mount root.
	configOverlayDir = "overlay"

	// EnvOperatorEndpoint names the container env var carrying the address
	// the agent dials to reach the operator's gRPC endpoint.
	EnvOperatorEndpoint = "SPAWNERY_OPERATOR_ENDPOINT"

	// TokenExpirationSeconds is the lifetime of the projected token. Short,
	// because it keeps the replay window small; the kubelet rotates it
	// well before it runs out.
	TokenExpirationSeconds int64 = 600
)

// DataClaimName is the name of the PVC of a persistent server.
func DataClaimName(server string) string {
	return server + "-" + DataVolumeName
}

// configVolume is the projected volume both BuildServerPod and BuildProxyPod
// mount read-only at ConfigMountPath: the group's rendered ConfigMap and the
// Network's forwarding secret. One function shared by both builders is what
// stops the two layers from drifting into different answers about where
// configuration lives.
func configVolume(groupConfigMap, forwardingSecret string) corev1.Volume {
	return corev1.Volume{
		Name: ConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: groupConfigMap},
							Items: []corev1.KeyToPath{
								{Key: ConfigValuesKey, Path: ConfigValuesKey},
							},
						},
					},
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: forwardingSecret},
							Items: []corev1.KeyToPath{
								{Key: ForwardingSecretKey, Path: configSecretFile},
							},
						},
					},
				},
			},
		},
	}
}

// configOverlayVolume is the volume ConfigOverlayVolumeName when a group
// declares spec.configOverlay, or nil when it does not — the caller appends
// it (and its mount) only in the non-nil case, since an always-present
// volume naming an empty ConfigMap is a pod that never starts, not an
// absent overlay.
//
// No Items: every key of the referenced ConfigMap becomes a file here,
// whatever its name, so a key internal/render does not recognise still
// reaches checkOverlayFiles and gets refused there — loudly, by design —
// instead of being filtered out by the kubelet before the renderer ever
// runs. See the comment on ConfigOverlayVolumeName for why an enumerated
// Items list was tried and rejected.
func configOverlayVolume(overlay *spawneryv1alpha1.ObjectRef) *corev1.Volume {
	if overlay == nil {
		return nil
	}
	return &corev1.Volume{
		Name: ConfigOverlayVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: overlay.Name},
			},
		},
	}
}

// BuildServerPod renders the pod of one Server. The Server owns the pod, so
// deleting the Server cascades.
func BuildServerPod(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
	agentEndpoint string,
) (*corev1.Pod, error) {
	if group.Spec.Image == "" {
		return nil, fmt.Errorf("server group %q has no image", group.Name)
	}
	if agentEndpoint == "" {
		return nil, fmt.Errorf("server group %q has no agent endpoint", group.Name)
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
		configVolume(GroupConfigMapName(group.Name), net.Spec.ForwardingSecretRef.Name),
	}
	mounts := []corev1.VolumeMount{
		{Name: DataVolumeName, MountPath: DataMountPath},
		{Name: TmpVolumeName, MountPath: TmpMountPath},
		{Name: AgentVolumeName, MountPath: AgentMountPath, ReadOnly: true},
		{Name: ConfigVolumeName, MountPath: ConfigMountPath, ReadOnly: true},
	}
	// Nested inside ConfigVolumeName's own mount: Kubernetes mounts a
	// VolumeMount whose path lies under another's without issue, ordering
	// them itself, and design spec 4.3's own DataMountPath+"/config" example
	// already relies on the same nesting elsewhere in this package.
	if vol := configOverlayVolume(group.Spec.ConfigOverlay); vol != nil {
		volumes = append(volumes, *vol)
		mounts = append(mounts, corev1.VolumeMount{
			Name:      ConfigOverlayVolumeName,
			MountPath: path.Join(ConfigMountPath, configOverlayDir),
			ReadOnly:  true,
		})
	}

	for _, m := range group.Spec.Mounts {
		if err := checkMountCollision(m); err != nil {
			return nil, err
		}
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
			{Name: EnvOperatorEndpoint, Value: agentEndpoint},
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
			// The pods carry no Kubernetes credentials from the API server's
			// own token machinery. AutomountServiceAccountToken stays off;
			// the projected, audience-bound token above is the exception,
			// and it is what ties the pod to ServiceAccountName below.
			ServiceAccountName:            ServerServiceAccountName,
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

// checkMountCollision refuses a user mount that reuses one of the operator's
// own volume names, or whose mount path collides with one of ours at the
// filesystem level. The API server would reject the resulting pod anyway —
// on a duplicate volume name outright — but a colliding path it happily
// accepts: Kubernetes permits nested mounts.
//
// The path check is deliberately asymmetric between the two mounts below and
// the other two, and that asymmetry is not an oversight to "tidy up" later:
//
//   - AgentMountPath and ConfigMountPath each get the full bidirectional
//     nesting check, equal path, nested under, or an ancestor of it, all
//     refused. They are the two of the four that hold something worth
//     shadowing: a user mount at AgentMountPath+"/token" would silently
//     overlay the exact file the agent reads its credential from, and a
//     mount at ConfigMountPath+"/forwarding.secret" would do the same to the
//     file the renderer reads the forwarding secret from. Nothing but this
//     check stops either. Nesting under either is never legitimate.
//   - DataMountPath and TmpMountPath only refuse an exact match (after
//     path.Clean, so a trailing slash does not slip past). Mounting AT
//     DataMountPath would replace the whole working directory and is
//     refused; mounting INSIDE it is the documented way to add extra files —
//     design spec 4.3's own ServerGroup example mounts a ConfigMap at
//     DataMountPath+"/config" — so unlike the other two, a nested path under
//     these two is a feature, not a collision.
//
// Path comparison is on segment boundaries, not raw string prefixes, so
// "/data-extra" is never mistaken for a child of "/data".
func checkMountCollision(m spawneryv1alpha1.Mount) error {
	for _, name := range []string{AgentVolumeName, ConfigVolumeName, ConfigOverlayVolumeName, DataVolumeName, TmpVolumeName} {
		if m.Name == name {
			return fmt.Errorf("mount %q reuses the reserved volume name %q", m.Name, name)
		}
	}

	user := path.Clean(m.MountPath)

	for _, reserved := range []string{AgentMountPath, ConfigMountPath} {
		clean := path.Clean(reserved)
		switch {
		case user == clean:
			return fmt.Errorf("mount %q targets the reserved mount path %q", m.Name, reserved)
		case isPathUnder(user, clean):
			return fmt.Errorf("mount %q at %q nests inside the reserved mount path %q", m.Name, m.MountPath, reserved)
		case isPathUnder(clean, user):
			return fmt.Errorf("mount %q at %q is an ancestor of the reserved mount path %q", m.Name, m.MountPath, reserved)
		}
	}

	for _, reserved := range []string{DataMountPath, TmpMountPath} {
		if user == path.Clean(reserved) {
			return fmt.Errorf("mount %q targets the reserved mount path %q", m.Name, reserved)
		}
	}
	return nil
}

// isPathUnder reports whether child is nested inside parent. It compares on
// path segment boundaries — appending a separator before the prefix check —
// so a sibling that merely shares a textual prefix, like "/data-extra" next
// to "/data", is never mistaken for a descendant. Both arguments must
// already be path.Clean-ed.
func isPathUnder(child, parent string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
}
