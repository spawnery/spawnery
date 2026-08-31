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
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// envNames is the container's env list in the order it was rendered. Order is
// asserted rather than membership because the whole point of appending is that
// the operator's own set stays where a reader expects it.
func envNames(pod *corev1.Pod) []string {
	out := make([]string, 0, len(pod.Spec.Containers[0].Env))
	for _, e := range pod.Spec.Containers[0].Env {
		out = append(out, e.Name)
	}
	return out
}

func TestAGroupsEnvIsAppendedAfterTheOperatorsOwn(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Env = []corev1.EnvVar{
			{Name: "JAVA_TOOL_OPTIONS", Value: "-Dgame.amountOfTeams=0"},
			{Name: "GAME_MODE", Value: "solo"},
		}
	})

	got := envNames(pod)
	want := []string{
		"SPAWNERY_NETWORK", "SPAWNERY_GROUP", "SPAWNERY_SERVER", EnvOperatorEndpoint,
		"JAVA_TOOL_OPTIONS", "GAME_MODE",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("env = %v, want %v", got, want)
	}

	// The value matters as much as the name. A group that sets a system
	// property its plugins read and gets an empty one has a server that starts
	// and behaves like a different group -- the failure this field exists to
	// make impossible, arriving through the field itself.
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "JAVA_TOOL_OPTIONS" && e.Value != "-Dgame.amountOfTeams=0" {
			t.Errorf("JAVA_TOOL_OPTIONS = %q, want the group's value", e.Value)
		}
	}
}

func TestAProxyGroupsEnvIsAppendedAfterTheOperatorsOwn(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Env = []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: "-Dvelocity.x=1"}}

	pod, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}

	got := envNames(pod)
	want := []string{
		"SPAWNERY_NETWORK", "SPAWNERY_GROUP", EnvProxy, EnvPlayerLimit,
		EnvFallbackGroups, EnvOperatorEndpoint, "JAVA_TOOL_OPTIONS",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

func TestAGroupWithNoEnvRendersExactlyWhatItRenderedBefore(t *testing.T) {
	// Every installation that never touches this field must get the pod it got
	// before, which is also what keeps the golden digests in hash_golden_test.go
	// still for them -- a changed digest there would replace every server in
	// every installation on the first reconcile after an upgrade, for a field
	// nobody set.
	server := envNames(build(t, nil))
	wantServer := []string{"SPAWNERY_NETWORK", "SPAWNERY_GROUP", "SPAWNERY_SERVER", EnvOperatorEndpoint}
	if strings.Join(server, ",") != strings.Join(wantServer, ",") {
		t.Errorf("server env = %v, want %v", server, wantServer)
	}

	proxy := envNames(buildProxy(t))
	wantProxy := []string{
		"SPAWNERY_NETWORK", "SPAWNERY_GROUP", EnvProxy, EnvPlayerLimit,
		EnvFallbackGroups, EnvOperatorEndpoint,
	}
	if strings.Join(proxy, ",") != strings.Join(wantProxy, ",") {
		t.Errorf("proxy env = %v, want %v", proxy, wantProxy)
	}
}

// The two hash tests are the ones that decide whether an edit to this field
// reaches a running fleet at all. Without them the operator would write the new
// env into the pod it renders for comparison, find every existing server's
// recorded hash unchanged, and leave the whole group running the old value --
// with the ServerGroup showing the new one.
func TestEnvReachesTheServerHash(t *testing.T) {
	net, group := testNetwork(), testGroup()
	before, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}

	group.Spec.Env = []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: "-Dgame.teamSize=4"}}
	after, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if before == after {
		t.Fatalf("the digest did not move when spec.env changed (%s): every server would keep the old value", before)
	}

	// And a different value is a different digest, not merely "set versus
	// unset". Solo and team differ by nothing but the value.
	group.Spec.Env = []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: "-Dgame.teamSize=3"}}
	other, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if other == after {
		t.Fatal("two different values digest the same")
	}
}

func TestEnvReachesTheProxyHash(t *testing.T) {
	net, group := testNetwork(), testProxyGroup()
	before, err := DesiredProxyHash(net, group, testEndpoint, nil)
	if err != nil {
		t.Fatalf("DesiredProxyHash: %v", err)
	}

	group.Spec.Env = []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: "-Dvelocity.x=1"}}
	after, err := DesiredProxyHash(net, group, testEndpoint, nil)
	if err != nil {
		t.Fatalf("DesiredProxyHash: %v", err)
	}
	if before == after {
		t.Fatalf("the digest did not move when spec.env changed (%s)", before)
	}
}

// TestTheReservedEnvPrefixMarkersMatchTheConstant reads the generated CRDs and
// checks that every spec.env CEL rule still denies exactly the prefix
// ReservedEnvPrefix names.
//
// The literal is spelled twice by necessity: a kubebuilder marker cannot
// interpolate a Go constant, so the prefix lives once in api/v1alpha1 and once
// in each marker. Renaming the constant would leave the markers denying the old
// prefix, and nothing else in this repository compares them -- the operator
// never reads the rule, and a CRD whose rule denies a prefix no pod uses
// installs and reconciles perfectly while letting a group shadow
// SPAWNERY_OPERATOR_ENDPOINT.
//
// The chart's copy is checked alongside config/crd/bases because the chart is
// what installations actually apply. They are written by the same `make
// manifests` run, so a divergence means somebody edited one by hand.
func TestTheReservedEnvPrefixMarkersMatchTheConstant(t *testing.T) {
	files := []string{
		"config/crd/bases/spawnery.cloud_servergroups.yaml",
		"config/crd/bases/spawnery.cloud_proxygroups.yaml",
		"charts/spawnery/templates/crds.yaml",
	}

	total := 0
	for _, rel := range files {
		raw, err := os.ReadFile(testenv.RepoPath(t, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		found := 0
		for _, doc := range strings.Split(string(raw), "\n---\n") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var crd map[string]any
			if err := yaml.Unmarshal([]byte(doc), &crd); err != nil {
				t.Fatalf("parse a document of %s: %v", rel, err)
			}
			for _, rule := range envValidationRules(crd) {
				found++
				total++
				if !strings.Contains(rule, "'"+spawneryv1alpha1.ReservedEnvPrefix+"'") {
					t.Errorf("%s: spec.env rule %q does not deny %q",
						rel, rule, spawneryv1alpha1.ReservedEnvPrefix)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s: no spec.env validation rule at all. Either the field lost its "+
				"marker -- in which case a group can shadow the operator's own variables -- "+
				"or it moved and this test has to follow it", rel)
		}
	}

	// Four: one for ServerGroup and one for ProxyGroup in config/crd/bases,
	// and both again in the chart's bundle. A number rather than "at least
	// one" so that a kind losing its rule cannot hide behind the others.
	if total != 4 {
		t.Errorf("found %d spec.env rules across %v, want 4", total, files)
	}
}

// envValidationRules returns the CEL rules on spec.env of a parsed CRD
// document, across every served version. Untyped map walking rather than
// apiextensions types: sigs.k8s.io/yaml is already a direct dependency of this
// module and k8s.io/apiextensions-apiserver is not, and this test needs one
// field out of the schema rather than the schema.
func envValidationRules(crd map[string]any) []string {
	var out []string
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	for _, v := range versions {
		version, _ := v.(map[string]any)
		node := dig(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "env")
		field, _ := node.(map[string]any)
		validations, _ := field["x-kubernetes-validations"].([]any)
		for _, item := range validations {
			entry, _ := item.(map[string]any)
			if rule, ok := entry["rule"].(string); ok {
				out = append(out, rule)
			}
		}
	}
	return out
}

func dig(node map[string]any, keys ...string) any {
	var current any = node
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}
