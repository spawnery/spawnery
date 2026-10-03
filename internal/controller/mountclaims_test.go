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
	// ConfigMap and Secret mounts must cost no API call: the reader holds
	// nothing, so any Get would fail the check.
	mounts := []spawneryv1alpha1.Mount{
		{Name: "motd", MountPath: "/data/motd", ConfigMap: &corev1.ConfigMapVolumeSource{}},
		{Name: "token", MountPath: "/data/token", Secret: &corev1.SecretVolumeSource{}},
	}
	if _, _, ok := checkMountClaims(context.Background(), pluginReader(t), "minecraft", mounts, false); !ok {
		t.Error("a group whose mounts name no claim was refused")
	}
}

func TestAClaimMountNeedsTheFlag(t *testing.T) {
	// A good claim is still refused; the remedy is the mount flag, not the
	// storage.
	c := pluginReader(t, pluginClaim("worlds", corev1.ReadWriteMany))

	reason, message, ok := checkMountClaims(context.Background(), c, "minecraft",
		[]spawneryv1alpha1.Mount{claimMount("worlds", "worlds")}, false)

	if ok {
		t.Fatal("a claim mount was served by an operator started without --allow-mount-volumes")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumesDisabled {
		t.Errorf("reason = %q, want ReasonMountVolumesDisabled", reason)
	}
	if !strings.Contains(message, "--allow-mount-volumes") {
		t.Errorf("message = %q, want it to name the flag", message)
	}
	if strings.Contains(message, "--allow-plugin-volumes") {
		t.Errorf("message = %q, still sends somebody to the plugin flag", message)
	}
	if !strings.Contains(message, "worlds") {
		t.Errorf("message = %q, want it to name the mount", message)
	}
}

func TestAClaimMountNeedsItsOwnFlagAndNotThePluginOne(t *testing.T) {
	// Through checkGroupVolumes, so the flag's wiring is under test;
	// --allow-plugin-volumes governs only extraPlugins.
	c := pluginReader(t, pluginClaim("worlds", corev1.ReadWriteMany))
	mounts := []spawneryv1alpha1.Mount{claimMount("worlds", "worlds")}

	reason, message, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		nil, nil, mounts, true, false, false)

	if ok {
		t.Fatal("a claim mount was accepted with only the plugin flag set")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumesDisabled {
		t.Errorf("reason %q, want %q", reason, spawneryv1alpha1.ReasonMountVolumesDisabled)
	}
	if !strings.Contains(message, "--allow-mount-volumes") {
		t.Errorf("the message does not name the flag to set: %s", message)
	}
	if strings.Contains(message, "--allow-plugin-volumes") {
		t.Errorf("the message still sends somebody to the plugin flag: %s", message)
	}
}

func TestAClaimMountIsAcceptedWithItsOwnFlag(t *testing.T) {
	c := pluginReader(t, pluginClaim("worlds", corev1.ReadWriteMany))
	mounts := []spawneryv1alpha1.Mount{claimMount("worlds", "worlds")}

	if _, _, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		nil, nil, mounts, false, false, true); !ok {
		t.Error("a claim mount was refused with its own flag set")
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
	// Otherwise the second pod sits Pending on volume affinity, and nothing
	// names the claim.
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
	// Naming whichever mount map iteration reached would make the condition
	// flap.
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
	// A group can set either field or both; every claim must be checked.
	c := pluginReader(t, pluginClaim("plugins", corev1.ReadWriteMany))

	reason, _, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, nil,
		[]spawneryv1alpha1.Mount{claimMount("worlds", "missing")},
		true, false, true)

	if ok {
		t.Fatal("a good extraPlugins claim carried a broken mount past the check")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumeUnusable {
		t.Errorf("reason = %q, want the mount's own reason rather than the plugin one", reason)
	}

	// extraPlugins is asked first, so its reason wins when both are wrong.
	reason, _, ok = checkGroupVolumes(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "missing"}, nil,
		[]spawneryv1alpha1.Mount{claimMount("worlds", "missing")},
		true, false, true)
	if ok {
		t.Fatal("two broken fields were accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Errorf("reason = %q, want ReasonPluginVolumeUnusable", reason)
	}
}
