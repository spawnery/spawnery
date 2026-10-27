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

package render

import (
	"os"
	"sort"
	"strings"
	"testing"
)

func TestCanvasAcceptsThreadedRegions(t *testing.T) {
	files, err := Canvas(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "threaded-regions:\n  threads: 4\n",
	})
	if err != nil {
		t.Fatalf("Canvas: %v", err)
	}
	if global := string(files["config/paper-global.yml"]); !strings.Contains(global, "threads: 4") {
		t.Errorf("threaded-regions.threads did not survive the render:\n%s", global)
	}
}

func TestPaperRefusesThreadedRegions(t *testing.T) {
	_, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "threaded-regions:\n  threads: 4\n",
	})
	if err == nil {
		t.Fatal("Paper accepted a section only Canvas declares")
	}
	if !strings.Contains(err.Error(), "threaded-regions") {
		t.Errorf("the refusal does not name the key: %v", err)
	}
}

func TestCanvasWritesThePaperFiles(t *testing.T) {
	files, err := Canvas(paperValues(), "s3cret", map[string]string{
		"paper-world-defaults.yml": "misc:\n  disable-end-credits: true\n",
	})
	if err != nil {
		t.Fatalf("Canvas: %v", err)
	}
	got := keysOf(files)
	want := append([]string(nil), PaperFiles...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Canvas wrote %v, want %v", got, want)
	}
}

func TestCanvasKeepsTheForwardingKeysCritical(t *testing.T) {
	files, err := Canvas(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "proxies:\n  velocity:\n    enabled: false\n",
	})
	if err != nil {
		t.Fatalf("Canvas: %v", err)
	}
	global := string(files["config/paper-global.yml"])
	for _, want := range []string{"enabled: true", "online-mode: true", "secret: s3cret"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q:\n%s", want, global)
		}
	}
}

func TestCanvasWorldDefaultsAcceptWhatOnlyCanvasDeclares(t *testing.T) {
	overlay := "chunks:\n  min-chunk-unload-count: 10\n"
	files, err := Canvas(paperValues(), "s3cret", map[string]string{
		"paper-world-defaults.yml": overlay,
	})
	if err != nil {
		t.Fatalf("Canvas: %v", err)
	}
	if !strings.Contains(string(files["config/paper-world-defaults.yml"]), "min-chunk-unload-count: 10") {
		t.Errorf("the key did not survive the render: %s", files["config/paper-world-defaults.yml"])
	}
	if _, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-world-defaults.yml": overlay,
	}); err == nil {
		t.Error("Paper accepted a world-defaults key only Canvas declares")
	}
}

func TestCanvasWritesTheKeysCanvasItselfReads(t *testing.T) {
	defaults, err := os.ReadFile(canvasGlobalDefault)
	if err != nil {
		t.Fatalf("read Canvas's own defaults: %v", err)
	}
	canvasKeys := velocityKeysOf(t, defaults, canvasGlobalDefault)

	files, err := Canvas(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Canvas: %v", err)
	}
	rendered := files["config/paper-global.yml"]
	for _, key := range sortedKeys(velocityKeysOf(t, rendered, "the rendered config/paper-global.yml")) {
		if !canvasKeys[key] {
			t.Errorf("the renderer writes proxies.velocity.%s, which Canvas does not declare:\n%s", key, rendered)
		}
	}
}
