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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The floor under a server's number is the API server's, so it holds against
// every writer rather than against the one path the reconciler writes through.

func numberedServer(ns string, number int32) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "hub-dvjk", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "hub"},
			Number:   number,
		},
	}
}

func TestTheAPIServerAdmitsAServersNumber(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := numberedServer(ns, 2)
	if err := c.Create(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	var read spawneryv1alpha1.Server
	if err := c.Get(ctx, client.ObjectKeyFromObject(srv), &read); err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.Spec.Number != 2 {
		t.Errorf("number = %d, want what was written", read.Spec.Number)
	}
}

func TestAServerWithoutANumberCarriesZero(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "hub-pgqg", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "hub"},
		},
	}
	if err := c.Create(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	var read spawneryv1alpha1.Server
	if err := c.Get(ctx, client.ObjectKeyFromObject(srv), &read); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Zero is the whole contract for a server nobody numbered: every reader
	// falls back on it rather than asking whether the field was set.
	if read.Spec.Number != 0 {
		t.Errorf("number = %d, want 0", read.Spec.Number)
	}
}

func TestTheAPIServerRefusesANegativeNumber(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	err := c.Create(ctx, numberedServer(ns, -1))
	if err == nil {
		t.Fatal("create accepted a negative number")
	}
	if !strings.Contains(err.Error(), "number") {
		t.Errorf("error does not name the field: %v", err)
	}
}
