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
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// fileReader is pluginReader under this file's name -- the fixture every test
// below builds on, empty unless given objects to seed.
func fileReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	return pluginReader(t, objects...)
}

// fileReaderWithClaim seeds a reader with a single claim named name, carrying
// the given access modes.
func fileReaderWithClaim(t *testing.T, name string, modes ...corev1.PersistentVolumeAccessMode) client.Reader {
	t.Helper()
	return pluginReader(t, pluginClaim(name, modes...))
}

func TestNoExtraFilesIsAlwaysFine(t *testing.T) {
	if _, _, ok := checkExtraFiles(context.Background(), fileReader(t), "minecraft", nil, false); !ok {
		t.Error("a group naming no claim was refused")
	}
}

func TestExtraFilesWithoutTheFlagIsRefused(t *testing.T) {
	c := fileReader(t)

	reason, message, ok := checkExtraFiles(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraFiles{ClaimName: "files"}, false)

	if ok {
		t.Fatal("a group was accepted on an installation that disabled the feature")
	}
	if reason != spawneryv1alpha1.ReasonFileVolumesDisabled {
		t.Errorf("reason %q, want %q", reason, spawneryv1alpha1.ReasonFileVolumesDisabled)
	}
	if !strings.Contains(message, "--allow-file-volumes") {
		t.Errorf("the message does not name the flag: %s", message)
	}
}

func TestAReadWriteOnceFilesClaimIsRefused(t *testing.T) {
	c := fileReaderWithClaim(t, "files", corev1.ReadWriteOnce)

	reason, message, ok := checkExtraFiles(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraFiles{ClaimName: "files"}, true)

	if ok {
		t.Fatal("a ReadWriteOnce claim was accepted")
	}
	if reason != spawneryv1alpha1.ReasonFileVolumeUnusable {
		t.Errorf("reason %q, want %q", reason, spawneryv1alpha1.ReasonFileVolumeUnusable)
	}
	if !strings.Contains(message, "ReadWriteMany") {
		t.Errorf("the message does not say what is needed: %s", message)
	}
}

func TestExtraFilesSwitchOffReadsNoClaim(t *testing.T) {
	// The other half of TestExtraFilesWithoutTheFlagIsRefused, and it needs
	// its own test because it is a claim about a call that does not happen.
	// An installation with the feature off must not spend an API read per
	// group per resync on a field it will refuse anyway -- see
	// TestTheSwitchOffReadsNoClaimAtAll in extraplugins_test.go, which this
	// mirrors.
	c := &countingReader{Reader: fileReaderWithClaim(t, "files", corev1.ReadWriteMany)}

	checkExtraFiles(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraFiles{ClaimName: "files"}, false)

	if c.gets != 0 {
		t.Errorf("the claim was read %d times on a disabled installation, want none", c.gets)
	}
}

func TestAGroupWithBothFieldsWrongReportsThePluginOneFirst(t *testing.T) {
	// checkGroupVolumes' existing order: extraPlugins is the older field and
	// the one more installations set, and when both are wrong there is no
	// reason to prefer the other.
	c := fileReader(t)

	reason, _, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"},
		&spawneryv1alpha1.ExtraFiles{ClaimName: "files"},
		nil, false, false)

	if ok {
		t.Fatal("both fields wrong was accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumesDisabled {
		t.Errorf("reason %q, want the plugin one first", reason)
	}
}
