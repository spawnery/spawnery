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
	// The overwhelmingly common case, and the one a regression here would
	// break for every installation that never asked for this.
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
	// The failure this replaces: the second server sits Pending with a
	// scheduling error about volume affinity, and nothing names the claim.
	c := pluginReader(t, pluginClaim("plugins", corev1.ReadWriteOnce))

	reason, message, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true)

	if ok {
		t.Fatal("a ReadWriteOnce claim was accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Errorf("reason = %q, want ReasonPluginVolumeUnusable", reason)
	}
	// The claim and the mode, because an administrator reading this has to be
	// able to fix it without guessing which of the two is wrong.
	if !strings.Contains(message, "plugins") || !strings.Contains(message, "ReadWriteMany") {
		t.Errorf("message = %q, want it to name the claim and the mode it needs", message)
	}
}

func TestAMissingClaimIsRefusedRatherThanMounted(t *testing.T) {
	// Kubernetes would leave the pod Pending forever on a claim that does not
	// exist. Refusing here puts the answer on the group, where somebody is
	// looking.
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

// countingReader records how many Gets reach it.
//
// It exists because "refuses before the claim is read" is a claim about a call
// that did not happen, and a test that only checks the returned reason cannot
// see one. Moving the switch below the Get leaves the reason correct and the
// read wasted, and the first version of the test below passed against exactly
// that.
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
	// An administrator whose claim is perfectly good has to be sent to the
	// operator's arguments and nowhere else.
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
	// The other half, and it needs its own test because it is a claim about a
	// call that does not happen. An installation with the feature off must not
	// spend an API read per group per resync on a field it will refuse anyway.
	c := &countingReader{Reader: pluginReader(t, pluginClaim("plugins", corev1.ReadWriteMany))}

	checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, false)

	if c.gets != 0 {
		t.Errorf("the claim was read %d times on a disabled installation, want none", c.gets)
	}
}

func TestAClaimWithSeveralModesIsAcceptedIfOneIsReadWriteMany(t *testing.T) {
	// accessModes is a list. A claim that is both RWO and RWX is mountable by
	// every node, and reading only the first entry would refuse it.
	c := pluginReader(t, pluginClaim("plugins", corev1.ReadWriteOnce, corev1.ReadWriteMany))

	if _, _, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true); !ok {
		t.Error("a claim listing ReadWriteMany among its modes was refused")
	}
}
