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
	"io"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/spawnery/spawnery/internal/testenv"
)

func TestSampleManifestIsAcceptedByTheAPIServer(t *testing.T) {
	c, ctx := testenv.Client(t)

	path := filepath.Join("..", "..", "config", "samples", "network.yaml")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

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
	if count != 5 {
		t.Fatalf("decoded %d documents from the sample, want 5 (Namespace, Secret, Network, ServerGroup, ProxyGroup)", count)
	}
}
