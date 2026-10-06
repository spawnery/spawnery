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

package main

import (
	"bytes"
	"testing"
)

func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"nonsense"}, func(string) string { return "" }, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestImportNeedsTheStore(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"import", "--world", "ns/g/k", "--dir", t.TempDir(), "--keep", "worlds/world"}, func(string) string { return "" }, &stderr)
	if code != 1 || !bytes.Contains(stderr.Bytes(), []byte("WORLDSYNC_ENDPOINT")) {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}
