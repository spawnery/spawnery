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

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func claimMount(name, claim string) spawneryv1alpha1.Mount {
	return spawneryv1alpha1.Mount{
		Name:                  name,
		MountPath:             "/data/" + name,
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: claim},
	}
}

func TestMountsWithNoClaimNeverTouchStorage(t *testing.T) {
	// The common case by a wide margin, and the one that must not start
	// costing an API call: a group whose mounts are all ConfigMaps and
	// Secrets. The reader holds nothing, so a Get of any kind would fail the
	// check and this test.
	mounts := []spawneryv1alpha1.Mount{
		{Name: "motd", MountPath: "/data/motd", ConfigMap: &corev1.ConfigMapVolumeSource{}},
		{Name: "token", MountPath: "/data/token", Secret: &corev1.SecretVolumeSource{}},
	}
	if _, _, ok := checkMountClaims(context.Background(), pluginReader(t), "minecraft", mounts, false); !ok {
		t.Error("a group whose mounts name no claim was refused")
	}
}

func TestAClaimMountNeedsTheFlag(t *testing.T) {
	// The claim exists and is perfectly good. It must still be refused, and
	// the message must send somebody to the operator's arguments rather than
	// to their storage -- the same rule spec.extraPlugins follows, because one
	// flag governs both.
	c := pluginReader(t, pluginClaim("worlds", corev1.ReadWriteMany))

	reason, message, ok := checkMountClaims(context.Background(), c, "minecraft",
		[]spawneryv1alpha1.Mount{claimMount("worlds", "worlds")}, false)

	if ok {
		t.Fatal("a claim mount was served by an operator started without --allow-plugin-volumes")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumesDisabled {
		t.Errorf("reason = %q, want ReasonMountVolumesDisabled", reason)
	}
	if !strings.Contains(message, "--allow-plugin-volumes") {
		t.Errorf("message = %q, want it to name the flag", message)
	}
	// And the mount, so that a group with eleven of them says which one.
	if !strings.Contains(message, "worlds") {
		t.Errorf("message = %q, want it to name the mount", message)
	}
}

func TestAReadWriteManyClaimMountIsAccepted(t *testing.T) {
	c := pluginReader(t, pluginClaim("worlds", corev1.ReadWriteMany))

	if _, _, ok := checkMountClaims(context.Background(), c, "minecraft",
		[]spawneryv1alpha1.Mount{claimMount("worlds", "worlds")}, true); !ok {
		t.Error("a ReadWriteMany claim mount was refused")
	}
}

func TestAReadWriteOnceClaimMountIsRefusedAndSaysWhy(t *testing.T) {
	// Same failure spec.extraPlugins guards against, reached through the other
	// field: the second pod of the group sits Pending on a scheduling error
	// about volume affinity, with nothing naming the claim.
	c := pluginReader(t, pluginClaim("worlds", corev1.ReadWriteOnce))

	reason, message, ok := checkMountClaims(context.Background(), c, "minecraft",
		[]spawneryv1alpha1.Mount{claimMount("worlds", "worlds")}, true)

	if ok {
		t.Fatal("a ReadWriteOnce claim mount was accepted")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumeUnusable {
		t.Errorf("reason = %q, want ReasonMountVolumeUnusable", reason)
	}
	if !strings.Contains(message, "worlds") || !strings.Contains(message, "ReadWriteMany") {
		t.Errorf("message = %q, want the claim and the mode it needs", message)
	}
}

func TestAMissingClaimMountIsRefused(t *testing.T) {
	reason, message, ok := checkMountClaims(context.Background(), pluginReader(t), "minecraft",
		[]spawneryv1alpha1.Mount{claimMount("worlds", "gone")}, true)

	if ok {
		t.Fatal("a mount naming a claim that does not exist was accepted")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumeUnusable {
		t.Errorf("reason = %q, want ReasonMountVolumeUnusable", reason)
	}
	if !strings.Contains(message, "gone") {
		t.Errorf("message = %q, want it to name the claim", message)
	}
}

func TestTheFirstBrokenClaimMountIsTheOneReported(t *testing.T) {
	// Two broken mounts, and the message names the first. Asserted rather than
	// left to chance because the alternative -- a message naming whichever the
	// map iteration reached -- would make the condition flap between two
	// sentences on a group nobody had touched.
	c := pluginReader(t, pluginClaim("second", corev1.ReadWriteOnce))
	mounts := []spawneryv1alpha1.Mount{
		claimMount("first", "missing"),
		claimMount("second", "second"),
	}

	_, message, ok := checkMountClaims(context.Background(), c, "minecraft", mounts, true)
	if ok {
		t.Fatal("two broken claim mounts were accepted")
	}
	if !strings.Contains(message, "first") {
		t.Errorf("message = %q, want the first broken mount", message)
	}
}

func TestBothVolumeFieldsAreCheckedTogether(t *testing.T) {
	// checkGroupVolumes is what the controllers call, and a group can set
	// either field or both. Without this, adding spec.mounts would have left
	// the claim it names unchecked on every path that only asked about
	// extraPlugins -- which is exactly the shape of the bug this closes.
	c := pluginReader(t, pluginClaim("plugins", corev1.ReadWriteMany))

	reason, _, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"},
		[]spawneryv1alpha1.Mount{claimMount("worlds", "missing")},
		true)

	if ok {
		t.Fatal("a good extraPlugins claim carried a broken mount past the check")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumeUnusable {
		t.Errorf("reason = %q, want the mount's own reason rather than the plugin one", reason)
	}

	// And the other way round: extraPlugins is asked first, so its reason wins
	// when both are wrong.
	reason, _, ok = checkGroupVolumes(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "missing"},
		[]spawneryv1alpha1.Mount{claimMount("worlds", "missing")},
		true)
	if ok {
		t.Fatal("two broken fields were accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Errorf("reason = %q, want ReasonPluginVolumeUnusable", reason)
	}
}
