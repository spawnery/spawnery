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

	// ServerConfigDirPath is the server's own configuration directory, and a
	// place no user mount may go -- not at it, and not inside it.
	//
	// **Measured in a kind cluster on 2026-08-31, because the design said the
	// opposite.** Design spec 4.3's own ServerGroup example mounts a ConfigMap
	// here, and checkMountCollision's comment cited it as the legitimate case
	// for nesting under /data. It is not legitimate: it breaks every start.
	//
	// The kubelet creates the parent directory of a mount itself, and creates
	// it root-owned and group-read-only -- drwxr-sr-x 0 10001 -- while fsGroup
	// with OnRootMismatch only ever touches the volume's own root, which comes
	// out drwxrwsrwx. So a mount anywhere under here leaves the container
	// unable to write into the directory, and the first thing it tries to
	// write is spawnery-config's own paper-global.yml:
	//
	//	spawnery-config: /data/config/paper-global.yml: open ...: permission denied
	//
	// The server then never starts, and nothing in that message names a mount.
	// Refusing it here is what turns that into a sentence on the object
	// somebody just wrote. Nothing this operator can do makes it work: the
	// ownership is the kubelet's, and fixing it would need a root init
	// container, which is the one thing every pod here is built not to have.
	//
	// Refused for proxy pods too, though only the Paper flavour writes here.
	// One rule rather than two, and a proxy loses nothing by it: Velocity
	// reads velocity.toml at /data's root and has no configuration directory
	// of its own.
	ServerConfigDirPath = DataMountPath + "/config"

	// PluginsMountPath is the server's plugins directory, and the one place
	// under DataMountPath a user mount cannot go.
	//
	// image/entrypoint.sh copies the agent jar into it on every start, and
	// every user mount is read-only (this package sets ReadOnly on all of
	// them, unconditionally), so a mount here makes that copy fail under
	// `set -eu` with a bare `cp:` message that names no cause. The jar cannot
	// simply be loaded from where it ships either: Paper writes its plugins'
	// data folders inside this directory, so pointing --plugins at a
	// read-only path takes Paper's own bundled plugins down with it.
	//
	// Refused on an exact match only, unlike the bidirectional check
	// AgentMountPath gets. A mount *inside* it is the ordinary way to add a
	// plugin and breaks nothing -- the copy writes one file beside whatever
	// is mounted -- and a mount above it is DataMountPath, which is already
	// refused on its own account.
	PluginsMountPath = DataMountPath + "/plugins"

	// PluginSourceVolumeName and PluginSourceMountPath are where a group's
	// spec.extraPlugins claim is mounted.
	//
	// **Outside DataMountPath, and that is the whole reason for a second
	// path.** A user mount may not target PluginsMountPath -- the comment
	// above says why -- and every mount this package renders is read-only, so
	// the claim cannot simply *be* the plugins directory. It is a source the
	// entrypoint copies out of, exactly as it copies the agent jar out of the
	// read-only part of the image.
	//
	// A constant known to both sides rather than a path the user chooses,
	// because the entrypoint has to find it. A chosen path would have to reach
	// the entrypoint through an environment variable, which is a second place
	// for the two to disagree.
	PluginSourceVolumeName = "extra-plugins"
	PluginSourceMountPath  = "/var/run/spawnery/plugins"

	// FileSourceVolumeName and FileSourceMountPath are where a group's
	// spec.extraFiles claim is mounted.
	//
	// Outside DataMountPath for the same reason PluginSourceMountPath is: the
	// claim cannot *be* the directory it fills, because every mount this
	// package renders is read-only and a read-only /data breaks everything
	// the server writes. It is a source the entrypoint copies out of.
	FileSourceVolumeName = "extra-files"
	FileSourceMountPath  = "/var/run/spawnery/files"

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
	//
	// "Applies to nothing else" is narrower than the reality now:
	// PluginSourceMountPath and FileSourceMountPath both nest under
	// AgentMountPath, so that one check refuses a colliding user mount over
	// three operator directories, not one. Those two are there wanting
	// exactly that refusal and inheriting it rather than repeating it — see
	// checkMountCollision, which spells their safety out as a dependency on
	// it. ConfigMountPath is still kept out from under it and carries its own
	// copy of the check, so that what each path refuses, and why, is readable
	// where that path is defined.
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

	// FSGroupID is the supplemental group the kubelet chowns DataVolumeName
	// to before the container starts, so uid 10001 — the container's own
	// uid, per nix/oci-common.nix's `uid` and `gid`, which set both to the
	// same 10001 — can write into a PersistentVolumeClaim that arrives
	// owned by root. It is not a separate identity: nix/oci-common.nix
	// gives the image one uid and one matching gid, both 10001, so the
	// value that must appear here is the same 10001 the container already
	// runs as.
	FSGroupID int64 = 10001
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
		configVolume(GroupConfigMapName(group.Name, RoleServer), net.Spec.ForwardingSecretRef.Name),
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

	userVolumes, userVolumeMounts, err := renderUserMounts(group.Spec.Mounts)
	if err != nil {
		return nil, err
	}
	volumes = append(volumes, userVolumes...)
	mounts = append(mounts, userVolumeMounts...)

	// The group's own plugin volume, if it named one. Read-only at the volume
	// as well as at the mount: one claim may serve several groups, and a group
	// that could write it could change what every other group loads.
	//
	// Mounted outside DataMountPath -- see PluginSourceMountPath. The
	// entrypoint copies out of it; it is not the plugins directory itself,
	// which a read-only mount could not be.
	if group.Spec.ExtraPlugins != nil {
		volumes = append(volumes, corev1.Volume{
			Name: PluginSourceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: group.Spec.ExtraPlugins.ClaimName,
					ReadOnly:  true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      PluginSourceVolumeName,
			MountPath: PluginSourceMountPath,
			ReadOnly:  true,
		})
	}

	// The group's own file volume, if it named one. Same reasoning as the
	// plugin source above: read-only at both the volume and the mount, and
	// outside DataMountPath because a read-only mount cannot be the directory
	// it fills.
	if group.Spec.ExtraFiles != nil {
		volumes = append(volumes, corev1.Volume{
			Name: FileSourceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: group.Spec.ExtraFiles.ClaimName,
					ReadOnly:  true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      FileSourceVolumeName,
			MountPath: FileSourceMountPath,
			ReadOnly:  true,
		})
	}

	container := corev1.Container{
		Name:  ContainerName,
		Image: group.Spec.Image,
		// Stdin, so `kubectl attach` can reach the console.
		//
		// Without it the container gets /dev/null on stdin, the server's
		// console reader sees EOF at once, and an attaching client's
		// keystrokes go nowhere -- which is exactly what a live 0.2.7 lobby
		// did before this was set. It is what lets an operator run /cloud on a
		// network where nobody has been granted a permission yet.
		//
		// StdinOnce is deliberately left false. It would close the container's
		// stdin the moment the first attaching client disconnects, so the
		// console would answer exactly one session and be dead for the rest of
		// the pod's life -- and the second person to try it would find a
		// command that used to work.
		//
		// No TTY either. Paper and Velocity both switch to a terminal console
		// when they have one, which changes how their output is written, and
		// nothing needs a terminal here: a command arrives over a plain pipe.
		Stdin: true,

		Ports: []corev1.ContainerPort{{
			Name:          MinecraftPortName,
			ContainerPort: MinecraftPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		// The group's own variables come last, after the four this operator
		// owns. Order is not what protects those four: ReservedEnvPrefix and
		// the CEL rule on spec.env are, and they make it impossible for a
		// group to repeat one of these names at all. Appending is a
		// readability decision -- it keeps the operator's own set at a fixed
		// position in every pod, so `kubectl describe pod` still reads
		// straight down for a group that sets twenty of its own.
		Env: append([]corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: "SPAWNERY_SERVER", Value: srv.Name},
			{Name: EnvOperatorEndpoint, Value: agentEndpoint},
		}, group.Spec.Env...),
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
			// FSGroup is set for every server pod, ephemeral and persistent
			// alike, not only for the persistent ones that need it. An
			// emptyDir already arrives world-writable, so an ephemeral pod
			// gains nothing from it — but a PersistentVolumeClaim arrives
			// owned by root, and uid 10001 (nix/oci-common.nix's `uid` and
			// `gid` for this image) cannot write into it without this. One
			// PodSecurityContext shape for every server pod, rather than a
			// second one that only a persistent group gets, is one fewer
			// thing to keep in sync as the two group types' pod specs
			// otherwise diverge — and the kubelet's ownership walk below
			// costs nothing extra on a freshly created, empty emptyDir.
			//
			// FSGroupChangePolicy is OnRootMismatch, not the kubelet's own
			// default of Always. Always recursively chowns every file under
			// the volume on every single pod start; for a Minecraft world
			// that can be gigabytes of region files, that cost is paid on
			// every restart forever. OnRootMismatch instead checks only the
			// volume's top-level directory: if its group already matches
			// FSGroup — true for an emptyDir after its first mount, and true
			// for a PVC after this fix's first chown — the kubelet skips the
			// walk entirely. The trade-off this accepts: a file deep in the
			// tree with the wrong group ownership (from a manual chmod,
			// say) is not corrected once the root already matches. Nothing
			// short of Always closes that, and Always is not affordable
			// here.
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

	// Stamped from the Network's status rather than computed here: one reader
	// of the Secret is the whole point (design section 2.1), and the group
	// controllers copy a string out of an object they already hold. Empty
	// means the operator does not know the digest yet, and an absent label is
	// "unknown" — see LabelForwardingHash.
	if hash := net.Status.ForwardingSecretHash; hash != "" {
		pod.Labels[LabelForwardingHash] = hash
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

// renderUserMounts turns spec.mounts into the volumes and mounts a pod
// carries. Shared by both group kinds: a ServerGroup and a ProxyGroup declare
// the same field with the same reserved paths, and two copies would be two
// answers the day one of them learns something.
//
// checkMountCollision sees one mount at a time, so a collision *between* two
// user mounts is structurally invisible to it. The API server catches both of
// these -- duplicate volume names and duplicate mount paths are invalid -- but
// it catches them as a rejected pod create, which reaches the user as a
// Degraded condition carrying an apimachinery validation message about an
// index in an array. Refusing here names the mount and the reason, and does it
// before anything is sent.
//
// A claim is the only source that can be writable, and it is read-only unless
// the mount says otherwise. ConfigMaps and Secrets are read-only whatever
// anybody writes -- the kubelet mounts them that way -- so asking about
// Writable for them would be a question with no answer. Nothing here checks
// that the claim exists or that it is ReadWriteMany; that needs a client, and
// internal/controller's checkMountClaims does it before this is ever reached.
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
		// Cleaned before comparing, so "/plugins" and "/plugins/" are one
		// path -- the same normalisation checkMountCollision applies before
		// comparing against the reserved paths.
		clean := path.Clean(m.MountPath)
		if seenPaths[clean] {
			return nil, nil, fmt.Errorf("mount %q targets %q, which another mount of this group already targets; one path can hold one mount", m.Name, m.MountPath)
		}
		seenPaths[clean] = true

		source := corev1.VolumeSource{ConfigMap: m.ConfigMap, Secret: m.Secret}
		readOnly := true
		if claim := m.PersistentVolumeClaim; claim != nil {
			readOnly = !claim.Writable
			// Read-only at the volume as well as at the mount, when it is
			// read-only at all. The pair matters: a volume marked writable and
			// mounted read-only is still attached read-write to the node, and
			// the difference shows up as a claim that cannot be attached
			// elsewhere rather than as anything about this pod.
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
//
//   - DataMountPath and TmpMountPath only refuse an exact match (after
//     path.Clean, so a trailing slash does not slip past). Mounting AT
//     DataMountPath would replace the whole working directory and is
//     refused; mounting INSIDE it is the documented way to add extra files,
//     so unlike the other two, a nested path under these two is a feature and
//     not a collision.
//
//     With one hole in it, which design spec 4.3 fell into: its own
//     ServerGroup example mounts a ConfigMap at DataMountPath+"/config", and
//     this comment used to cite that example as the legitimate case. It is
//     not one. ServerConfigDirPath carries what was measured and why that
//     path is now refused equal-or-under like the two above.
//
//   - FileSourceMountPath and PluginSourceMountPath get no entry of their own,
//     and must not be given one. Both are directories *under* AgentMountPath
//     — "/var/run/spawnery/files" and "/var/run/spawnery/plugins" — so the
//     bidirectional check above has already refused a mount at either of them,
//     anywhere inside either of them, and at any ancestor of either, before
//     control reaches the exact-match loop below. An entry there could never
//     fire.
//
//     That is a dependency, not a coincidence, and it is the whole reason
//     these two claims are safe: nothing else refuses a spec.mounts entry
//     that would shadow the claim an entrypoint copies out of. If
//     AgentMountPath ever stops being a parent of these two, or its check is
//     narrowed to an exact match, each of them needs its own bidirectional
//     entry here on the same day. TestCollidingUserMountsAreRefused pins the
//     refusal for FileSourceMountPath and for a path nested under it, so that
//     narrowing fails a test rather than quietly opening the hole.
//
// Path comparison is on segment boundaries, not raw string prefixes, so
// "/data-extra" is never mistaken for a child of "/data".
func checkMountCollision(m spawneryv1alpha1.Mount) error {
	for _, name := range []string{AgentVolumeName, ConfigVolumeName, ConfigOverlayVolumeName, DataVolumeName, TmpVolumeName, FileSourceVolumeName} {
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

	// Equal or under, like the two above rather than like the two below: a
	// mount AT this directory replaces the one the server writes into, and a
	// mount inside it makes that directory unwritable. See ServerConfigDirPath
	// for the measurement.
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

	// Its own message rather than the generic one above, because the generic
	// one would send a reader looking for what the operator keeps there. What
	// it keeps there is one file it writes on every start, and the remedy is
	// to mount a directory beside it rather than over it.
	if user == path.Clean(PluginsMountPath) {
		return fmt.Errorf(
			"mount %q targets %s, where the entrypoint copies the agent plugin on every start; "+
				"a read-only mount there fails that copy and the server does not come up. "+
				"Mount inside it instead, at %s/<name>",
			m.Name, PluginsMountPath, PluginsMountPath)
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
