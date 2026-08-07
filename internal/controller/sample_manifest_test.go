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
	"io"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/spawnery/spawnery/internal/testenv"
)

// TestSampleManifestIsAcceptedByTheAPIServer decodes config/samples/network.yaml
// and creates every object in it against the envtest control plane. This
// environment has no container runtime to run the brief's k3d smoke test, so
// this is the check available here that the shipped sample is not garbage: the
// structural schema and the CEL rules on Network and ServerGroup both run
// server-side, exactly as they would against a real cluster. It cannot prove
// the pod actually gets scheduled — envtest runs no kubelet — only that every
// object in the sample is accepted.
func TestSampleManifestIsAcceptedByTheAPIServer(t *testing.T) {
	c, ctx := testenv.Client(t)

	path := filepath.Join("..", "..", "config", "samples", "network.yaml")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	decoder := apimachineryyaml.NewYAMLOrJSONDecoder(f, 4096)
	count := 0
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode document %d: %v", count+1, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		count++
		if err := c.Create(ctx, &obj); err != nil {
			t.Errorf("create %s %s/%s: %v", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
	if count != 4 {
		t.Fatalf("decoded %d documents from the sample, want 4 (Namespace, Secret, Network, ServerGroup)", count)
	}
}
