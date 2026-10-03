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
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func pluginClaim(name string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: modes},
	}
}

func pluginReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestNoExtraPluginsIsAccepted(t *testing.T) {
	if _, _, ok := checkExtraPlugins(context.Background(), pluginReader(t), "minecraft", nil, false); !ok {
		t.Error("a group with no extraPlugins was refused")
	}
}

func TestAReadWriteManyClaimIsAccepted(t *testing.T) {
	c := pluginReader(t, pluginClaim("plugins", corev1.ReadWriteMany))

	if _, _, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true); !ok {
		t.Error("a ReadWriteMany claim was refused")
	}
}

func TestAReadWriteOnceClaimIsRefusedAndSaysWhy(t *testing.T) {
	// Otherwise the second server sits Pending on volume affinity, and
	// nothing names the claim.
	c := pluginReader(t, pluginClaim("plugins", corev1.ReadWriteOnce))

	reason, message, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true)

	if ok {
		t.Fatal("a ReadWriteOnce claim was accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Errorf("reason = %q, want ReasonPluginVolumeUnusable", reason)
	}
	if !strings.Contains(message, "plugins") || !strings.Contains(message, "ReadWriteMany") {
		t.Errorf("message = %q, want it to name the claim and the mode it needs", message)
	}
}

func TestAMissingClaimIsRefusedRatherThanMounted(t *testing.T) {
	// Kubernetes would leave the pod Pending forever on a missing claim.
	c := pluginReader(t)

	reason, message, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "absent"}, true)

	if ok {
		t.Fatal("a claim that does not exist was accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Errorf("reason = %q, want ReasonPluginVolumeUnusable", reason)
	}
	if !strings.Contains(message, "absent") {
		t.Errorf("message = %q, want it to name the claim", message)
	}
}

// countingReader counts Gets, so a test can assert a read that did not happen.
type countingReader struct {
	client.Reader
	gets int
}

func (c *countingReader) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	c.gets++
	return c.Reader.Get(ctx, key, obj, opts...)
}

func TestTheSwitchOffRefusesAndNamesTheFlagRatherThanTheClaim(t *testing.T) {
	c := pluginReader(t)

	reason, message, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "absent"}, false)

	if ok {
		t.Fatal("extraPlugins was accepted on an installation that disabled it")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumesDisabled {
		t.Errorf("reason = %q, want ReasonPluginVolumesDisabled", reason)
	}
	if !strings.Contains(message, "--allow-plugin-volumes") {
		t.Errorf("message = %q, want it to name the flag", message)
	}
}

func TestTheSwitchOffReadsNoClaimAtAll(t *testing.T) {
	// With the feature off, no API read per group per resync on a field it
	// refuses anyway.
	c := &countingReader{Reader: pluginReader(t, pluginClaim("plugins", corev1.ReadWriteMany))}

	checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, false)

	if c.gets != 0 {
		t.Errorf("the claim was read %d times on a disabled installation, want none", c.gets)
	}
}

func TestAClaimWithSeveralModesIsAcceptedIfOneIsReadWriteMany(t *testing.T) {
	// A claim that is both RWO and RWX is mountable by every node.
	c := pluginReader(t, pluginClaim("plugins", corev1.ReadWriteOnce, corev1.ReadWriteMany))

	if _, _, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true); !ok {
		t.Error("a claim listing ReadWriteMany among its modes was refused")
	}
}

// failingReader exists because a fake client cannot be told to fail.
type failingReader struct {
	client.Reader
	err error
}

func (r failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func TestAClaimTheAPIServerDidNotAnswerAboutIsNoVerdict(t *testing.T) {
	// The reader is uncached; an API blip must not read as "unusable" and
	// condemn the group's servers.
	reader := failingReader{pluginReader(t), errors.New("the server is currently unable to handle the request")}
	reason, _, ok := checkGroupVolumes(context.Background(), reader, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, nil, nil, true, true, true)
	if ok || reason != reasonClaimUnreadable {
		t.Fatalf("reason = %q ok = %v, want the unreadable marker and no verdict", reason, ok)
	}

	accepted := []metav1.Condition{{Type: spawneryv1alpha1.ConditionAccepted, Status: metav1.ConditionTrue,
		Reason: spawneryv1alpha1.ReasonAccepted}}
	if _, _, ok := keepLastVolumeDecision(accepted, reason, "x", false); !ok {
		t.Error("an accepted group lost its acceptance to a read error")
	}
	refused := []metav1.Condition{{Type: spawneryv1alpha1.ConditionAccepted, Status: metav1.ConditionFalse,
		Reason: spawneryv1alpha1.ReasonPluginVolumeUnusable, Message: "claim \"plugins\" does not exist"}}
	r2, m2, ok := keepLastVolumeDecision(refused, reason, "x", false)
	if ok || r2 != spawneryv1alpha1.ReasonPluginVolumeUnusable || m2 == "x" {
		t.Errorf("a refused group's decision was not kept: %q %q %v", r2, m2, ok)
	}
	if r3, _, ok := keepLastVolumeDecision(accepted, spawneryv1alpha1.ReasonMountVolumeUnusable, "m", false); ok || r3 != spawneryv1alpha1.ReasonMountVolumeUnusable {
		t.Error("a real refusal was rewritten")
	}
}

func TestAnImageSourceNeedsNoSwitchAndNoClaim(t *testing.T) {
	reason, message, ok := checkExtraPlugins(context.Background(), pluginReader(t), "games",
		&spawneryv1alpha1.ExtraPlugins{Image: "registry.example.net/lobby-plugins:1"}, false)
	if !ok {
		t.Fatalf("an image source was refused with the plugin volumes switch off: %s %s", reason, message)
	}
	reason, message, ok = checkExtraFiles(context.Background(), pluginReader(t), "games",
		&spawneryv1alpha1.ExtraFiles{Image: "registry.example.net/lobby-files:1"}, false)
	if !ok {
		t.Fatalf("an image file source was refused: %s %s", reason, message)
	}
}
