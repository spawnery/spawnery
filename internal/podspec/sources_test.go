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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func TestAnImageSourceIsAReadOnlyImageVolume(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ExtraPlugins = &spawneryv1alpha1.ExtraPlugins{Image: "registry.example.net/lobby-plugins:1"}
		g.Spec.ExtraFiles = &spawneryv1alpha1.ExtraFiles{Image: "registry.example.net/lobby-files:1", PullPolicy: corev1.PullAlways}
	})
	plugins := volumeNamed(pod, PluginSourceVolumeName)
	if plugins == nil || plugins.Image == nil || plugins.Image.Reference != "registry.example.net/lobby-plugins:1" {
		t.Fatalf("plugin source = %+v, want an image volume", plugins)
	}
	if plugins.Image.PullPolicy != corev1.PullIfNotPresent {
		t.Errorf("empty pull policy rendered %q, want IfNotPresent", plugins.Image.PullPolicy)
	}
	files := volumeNamed(pod, FileSourceVolumeName)
	if files == nil || files.Image == nil || files.Image.PullPolicy != corev1.PullAlways {
		t.Fatalf("file source = %+v, want an image volume with Always", files)
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if (m.Name == PluginSourceVolumeName || m.Name == FileSourceVolumeName) && !m.ReadOnly {
			t.Errorf("%s is mounted writable", m.Name)
		}
	}
}

func TestSubstitutionReachesTheContainerAsItsPrefix(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Substitution = &spawneryv1alpha1.Substitution{Prefix: "SECRET_"}
	})
	found := false
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvSubstitutionPrefix {
			found = e.Value == "SECRET_"
		}
	}
	if !found {
		t.Errorf("%s=SECRET_ missing from %v", EnvSubstitutionPrefix, pod.Spec.Containers[0].Env)
	}
	if bare := build(t, nil); envHas(bare, EnvSubstitutionPrefix) {
		t.Error("a group without substitution got the prefix variable")
	}
}

func TestKeepReachesTheContainerOneEntryPerLine(t *testing.T) {
	storage := &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi"), Keep: []string{"world", "plugins/ExampleGame/state"}}
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Storage = storage
	})
	var got string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvKeep {
			got = e.Value
		}
	}
	if got != "world\nplugins/ExampleGame/state" {
		t.Errorf("%s = %q", EnvKeep, got)
	}

	storage.Keep = nil
	if bare := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Storage = storage
	}); envHas(bare, EnvKeep) {
		t.Error("a group without keep got the variable")
	}
	if envHas(build(t, nil), EnvKeep) {
		t.Error("a group without storage got the variable")
	}
}

func TestReplaceReachesTheContainerOneEntryPerLine(t *testing.T) {
	storage := &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi"),
		Keep: []string{"world"}, Replace: []string{"worlds/templates", "worlds/arena"}}
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Storage = storage
	})
	var got string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvReplace {
			got = e.Value
		}
	}
	if got != "worlds/templates\nworlds/arena" {
		t.Errorf("%s = %q", EnvReplace, got)
	}

	storage.Replace = nil
	if bare := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Storage = storage
	}); envHas(bare, EnvReplace) {
		t.Error("a group without replace got the variable")
	}
}

func TestAChangedImageReferenceMovesThePodHash(t *testing.T) {
	net, group := testNetwork(), testGroup()
	group.Spec.ExtraPlugins = &spawneryv1alpha1.ExtraPlugins{Image: "registry.example.net/lobby-plugins@sha256:" + sixtyFour("a")}
	before, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	group.Spec.ExtraPlugins.Image = "registry.example.net/lobby-plugins@sha256:" + sixtyFour("b")
	after, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("a new digest left the hash unchanged, so publishing it would roll nothing")
	}
}

func envHas(pod *corev1.Pod, name string) bool {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func sixtyFour(c string) string {
	out := ""
	for i := 0; i < 64; i++ {
		out += c
	}
	return out
}

func TestAProxyImageSourceIsAReadOnlyImageVolume(t *testing.T) {
	g := testProxyGroup()
	g.Spec.ExtraPlugins = &spawneryv1alpha1.ExtraPlugins{Image: "registry.example.net/gateway-plugins:1"}
	g.Spec.ExtraFiles = &spawneryv1alpha1.ExtraFiles{Image: "registry.example.net/gateway-files:1"}
	g.Spec.Substitution = &spawneryv1alpha1.Substitution{Prefix: "SECRET_"}
	pod, err := BuildProxyPod(testNetwork(), g, "gateway-abcd", testEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{PluginSourceVolumeName, FileSourceVolumeName} {
		v := volumeNamed(pod, name)
		if v == nil || v.Image == nil || v.Image.PullPolicy != corev1.PullIfNotPresent {
			t.Errorf("%s = %+v, want an image volume with IfNotPresent", name, v)
		}
	}
	if proxyEnv(pod, EnvSubstitutionPrefix) != "SECRET_" {
		t.Errorf("the proxy did not get the substitution prefix")
	}
}
