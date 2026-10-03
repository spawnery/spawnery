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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	MinecraftPort     int32 = 25565
	MinecraftPortName       = "minecraft"

	ContainerName = "minecraft"

	// DataVolumeName is an emptyDir for ephemeral groups, a PVC for persistent ones.
	DataVolumeName = "data"
	// TmpVolumeName is scratch space; the root filesystem is read-only.
	TmpVolumeName = "tmp"

	DataMountPath = "/data"
	TmpMountPath  = "/tmp"

	// ServerConfigDirPath refuses user mounts at or under it: the kubelet creates
	// a mount's parent directories root-owned and group-read-only, and fsGroup
	// with OnRootMismatch only fixes the volume root, so the server could no
	// longer write paper-global.yml here and never starts. Refused for proxy
	// pods too; Velocity has no configuration directory to lose.
	ServerConfigDirPath = DataMountPath + "/config"

	// PluginsMountPath refuses a user mount at exactly this path: the entrypoint
	// copies the agent jar in on every start and every user mount is read-only.
	// The jar cannot load from elsewhere, because Paper writes its plugins' data
	// folders here. A mount inside it is the ordinary way to add a plugin.
	PluginsMountPath = DataMountPath + "/plugins"

	// PluginSourceVolumeName and PluginSourceMountPath are where spec.extraPlugins
	// is mounted: outside DataMountPath, as a read-only source the entrypoint
	// copies out of, since the plugins directory itself cannot be read-only. A
	// fixed path, so the entrypoint needs no env var to find it.
	PluginSourceVolumeName = "extra-plugins"
	PluginSourceMountPath  = "/var/run/spawnery/plugins"

	// FileSourceVolumeName and FileSourceMountPath are where spec.extraFiles is
	// mounted, outside DataMountPath for the same reason.
	FileSourceVolumeName = "extra-files"
	FileSourceMountPath  = "/var/run/spawnery/files"

	// SLPHealthBinary exists because the kubelet has no SLP probe, and a
	// tcpSocket probe on 25565 turns green before the world is loaded.
	SLPHealthBinary = "/usr/local/bin/spawnery-slp"

	// AgentVolumeName carries the agent's token and the CA it verifies the
	// operator's gRPC endpoint with.
	AgentVolumeName = "spawnery-agent"
	AgentMountPath  = "/var/run/spawnery"
	// AgentTokenPath and AgentCAPath are relative to AgentMountPath.
	AgentTokenPath = "token"
	AgentCAPath    = "ca.crt"

	// ConfigVolumeName carries the group's ConfigMap and the Network's forwarding
	// secret in the layout internal/render.Load reads by default.
	ConfigVolumeName = "spawnery-config"
	// ConfigOverlayVolumeName is a plain ConfigMap volume rather than a source in
	// ConfigVolumeName's projection: a projected source surfaces only the keys
	// named in Items, so a misnamed overlay key would vanish silently instead of
	// reaching internal/render's checkOverlayFiles and being refused.
	ConfigOverlayVolumeName = "spawnery-config-overlay"
	// ConfigMountPath is not /data/config, where Paper writes its own files and a
	// read-only ConfigMap would break the start, and not under AgentMountPath, so
	// each carries its own bidirectional check in checkMountCollision.
	ConfigMountPath = "/etc/spawnery"
	// ConfigValuesKey is both the ConfigMap data key and the file name under
	// ConfigMountPath; it matches internal/render.ValuesFile.
	ConfigValuesKey     = "config.yaml"
	ForwardingSecretKey = "secret"
	// configSecretFile duplicates internal/render.SecretFile so that podspec does
	// not import a package that touches the filesystem.
	configSecretFile = "forwarding.secret"
	// configOverlayDir duplicates internal/render.OverlayDir, for the same reason.
	configOverlayDir = "overlay"

	EnvOperatorEndpoint = "SPAWNERY_OPERATOR_ENDPOINT"

	// TokenExpirationSeconds is short to keep the replay window small; the
	// kubelet rotates the token well before it runs out.
	TokenExpirationSeconds int64 = 600

	// FSGroupID is the image's uid and gid (nix/oci-common.nix), so the
	// container can write into a PVC that arrives owned by root.
	FSGroupID int64 = 10001
)

func DataClaimName(server string) string {
	return server + "-" + DataVolumeName
}

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

// configOverlayVolume returns nil without spec.configOverlay: a volume naming
// an empty ConfigMap is a pod that never starts. No Items, see
// ConfigOverlayVolumeName.
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

// BuildServerPod's pod is owned by the Server, so deleting the Server cascades.
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

	scheduling := EffectiveScheduling(net, group.Spec.Scheduling)

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
		configVolume(GroupConfigMapName(group.Name, RoleServer), net.Spec.ForwardingSecretRef.Name),
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
	// groups, and a group that could write it could change what the others load.
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

	container := corev1.Container{
		Name:  ContainerName,
		Image: group.Spec.Image,
		// Stdin lets `kubectl attach` reach the console. StdinOnce would close stdin
		// after the first attached client leaves; a TTY would switch Paper and
		// Velocity to terminal console output.
		Stdin: true,

		Ports: []corev1.ContainerPort{{
			Name:          MinecraftPortName,
			ContainerPort: MinecraftPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		// The group's own variables come last; ReservedEnvPrefix and the CEL rule on
		// spec.env, not order, protect the operator's four.
		Env: append(append(append([]corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: "SPAWNERY_SERVER", Value: srv.Name},
			{Name: EnvOperatorEndpoint, Value: agentEndpoint},
		}, substitutionEnv(group.Spec.Substitution)...), keepEnv(group.Spec.Storage)...), group.Spec.Env...),
		VolumeMounts: mounts,
		// Readiness only: a liveness restart would kick every player, whereas the
		// state machine deregisters on a red readiness probe.
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
			Containers: []corev1.Container{container},
			Volumes:    volumes,
			// Never for an ephemeral server: Always would restart the container over
			// the same emptyDir and bring a finished round back with its own world.
			RestartPolicy: restartPolicy(group),
			// The projected, audience-bound token above is the only credential the pod
			// carries; AutomountServiceAccountToken stays off.
			ServiceAccountName:            ServerServiceAccountName,
			AutomountServiceAccountToken:  ptr.To(false),
			ImagePullSecrets:              pullSecrets,
			TerminationGracePeriodSeconds: ptr.To(group.Spec.TerminationGracePeriodSeconds),
			// FSGroup is set for every server pod: a PVC arrives owned by root, and one
			// PodSecurityContext shape is simpler than two. OnRootMismatch rather than
			// Always, which would chown gigabytes of region files on every start; the
			// cost is that a deep file with the wrong group is not corrected.
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

func restartPolicy(group *spawneryv1alpha1.ServerGroup) corev1.RestartPolicy {
	if group.Spec.Type == spawneryv1alpha1.ServerGroupPersistent {
		return corev1.RestartPolicyAlways
	}
	return corev1.RestartPolicyNever
}

// keepsWorld reports whether a server's world outlives its pod: persistent
// servers and on-demand members.
func keepsWorld(group *spawneryv1alpha1.ServerGroup) bool {
	return group.Spec.Type == spawneryv1alpha1.ServerGroupPersistent ||
		group.Spec.Type == spawneryv1alpha1.ServerGroupOnDemand
}

func dataVolume(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) corev1.Volume {
	if keepsWorld(group) {
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

// renderUserMounts is shared by ServerGroup and ProxyGroup. It refuses
// duplicate names and paths among user mounts itself, because the API
// server would only reject the pod with an array index. Claim existence and
// access mode are checked by internal/controller's checkMountClaims.
func renderUserMounts(list []spawneryv1alpha1.Mount) ([]corev1.Volume, []corev1.VolumeMount, error) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	seenNames := make(map[string]bool, len(list))
	seenPaths := make(map[string]bool, len(list))
	for _, m := range list {
		if err := checkMountCollision(m); err != nil {
			return nil, nil, err
		}
		if seenNames[m.Name] {
			return nil, nil, fmt.Errorf("mount %q is declared twice; two mounts of one group cannot share a name", m.Name)
		}
		seenNames[m.Name] = true
		clean := path.Clean(m.MountPath)
		if seenPaths[clean] {
			return nil, nil, fmt.Errorf("mount %q targets %q, which another mount of this group already targets; one path can hold one mount", m.Name, m.MountPath)
		}
		seenPaths[clean] = true

		source := corev1.VolumeSource{ConfigMap: m.ConfigMap, Secret: m.Secret}
		readOnly := true
		if claim := m.PersistentVolumeClaim; claim != nil {
			readOnly = !claim.Writable
			// Read-only at the volume as well: a writable volume mounted read-only is
			// still attached read-write to the node.
			source = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim.ClaimName,
					ReadOnly:  readOnly,
				},
			}
		}
		volumes = append(volumes, corev1.Volume{Name: m.Name, VolumeSource: source})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			SubPath:   m.SubPath,
			ReadOnly:  readOnly,
		})
	}
	return volumes, mounts, nil
}

var reservedVolumeNames = []string{
	AgentVolumeName, ConfigVolumeName, ConfigOverlayVolumeName, DataVolumeName,
	TmpVolumeName, FileSourceVolumeName, PluginSourceVolumeName,
}

// checkMountCollision refuses a user mount that reuses an operator volume
// name or collides with an operator path; the API server accepts nested
// mounts. AgentMountPath and ConfigMountPath refuse equal, under and above,
// since a mount there would shadow a credential. DataMountPath and
// TmpMountPath refuse only an exact match, as mounting inside them is a
// feature; ServerConfigDirPath is the exception. FileSourceMountPath and
// PluginSourceMountPath have no entry because they live under
// AgentMountPath and rely on its check; if that ever changes they need
// their own (TestCollidingUserMountsAreRefused pins it).
func checkMountCollision(m spawneryv1alpha1.Mount) error {
	for _, name := range reservedVolumeNames {
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

	if conf := path.Clean(ServerConfigDirPath); user == conf || isPathUnder(user, conf) {
		return fmt.Errorf("mount %q at %q is at or inside %s, the directory the server writes its "+
			"own configuration into; the kubelet creates a mount's parent directory root-owned, "+
			"so this leaves the server unable to write %s/paper-global.yml and it never starts. "+
			"Use spec.configOverlay for server.properties, paper-global.yml and "+
			"paper-world-defaults.yml",
			m.Name, m.MountPath, ServerConfigDirPath, ServerConfigDirPath)
	}

	for _, reserved := range []string{DataMountPath, TmpMountPath} {
		if user == path.Clean(reserved) {
			return fmt.Errorf("mount %q targets the reserved mount path %q", m.Name, reserved)
		}
	}

	if user == path.Clean(PluginsMountPath) {
		return fmt.Errorf(
			"mount %q targets %s, where the entrypoint copies the agent plugin on every start; "+
				"a read-only mount there fails that copy and the server does not come up. "+
				"Mount inside it instead, at %s/<name>",
			m.Name, PluginsMountPath, PluginsMountPath)
	}
	return nil
}

// isPathUnder compares on segment boundaries, so "/data-extra" is not under
// "/data". Both arguments must be path.Clean-ed.
func isPathUnder(child, parent string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
}
