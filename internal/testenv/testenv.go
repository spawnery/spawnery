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

// Package testenv boots a shared envtest control plane for controller tests.
package testenv

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

var (
	once sync.Once
	env  *envtest.Environment
	cfg  *rest.Config
	sch  *runtime.Scheme
	boot error
)

// crdPath walks up from the test's working directory until it finds
// config/crd/bases, so tests work from any package.
func crdPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "config", "crd", "bases")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("config/crd/bases not found — run 'make manifests' first")
		}
		dir = parent
	}
}

// RepoPath resolves a repository-relative path by walking up from the test's
// working directory until it finds go.mod. Tests run with their package
// directory as the working directory, so a plain relative path would break as
// soon as a test moves to another package.
func RepoPath(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Config starts the shared control plane on first use and returns its config.
func Config(t *testing.T) *rest.Config {
	t.Helper()
	once.Do(func() {
		sch = runtime.NewScheme()
		if boot = clientgoscheme.AddToScheme(sch); boot != nil {
			return
		}
		if boot = spawneryv1alpha1.AddToScheme(sch); boot != nil {
			return
		}
		env = &envtest.Environment{
			CRDDirectoryPaths:     []string{crdPath(t)},
			ErrorIfCRDPathMissing: true,
		}
		cfg, boot = env.Start()
	})
	if boot != nil {
		t.Fatalf("start envtest: %v", boot)
	}
	return cfg
}

// Scheme returns the scheme the shared control plane was started with.
func Scheme(t *testing.T) *runtime.Scheme {
	Config(t)
	return sch
}

// Client returns a client against the shared control plane, plus a context
// cancelled at the end of the test.
func Client(t *testing.T) (client.Client, context.Context) {
	t.Helper()
	c, err := client.New(Config(t), client.Options{Scheme: Scheme(t)})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return c, ctx
}

// Namespace creates a unique namespace for one test and returns its name.
// Every test gets its own, so the one-network-per-namespace rule and the
// group names never collide across tests.
func Namespace(t *testing.T, ctx context.Context, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "spawnery-test-"},
	}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

// Stop tears the control plane down. Call it from TestMain of every test
// package that uses testenv, once all tests in that package have run.
func Stop() error {
	if env == nil {
		return nil
	}
	return env.Stop()
}
