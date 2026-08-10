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

package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func proxyGroup(ns string, expose spawneryv1alpha1.ExposeSpec) *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.1.0",
			Expose:     expose,
			Routing: spawneryv1alpha1.RoutingSpec{
				FallbackGroups: []string{"lobby"},
			},
		},
	}
}

func TestProxyGroupExposeValidation(t *testing.T) {
	c, ctx := testenv.Client(t)

	cases := []struct {
		name    string
		expose  spawneryv1alpha1.ExposeSpec
		wantErr bool
	}{
		{
			name:   "loadbalancer without sub-block",
			expose: spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer},
		},
		{
			name: "nodeport with matching sub-block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30565},
			},
		},
		{
			name: "hostport with matching sub-block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
		},
		{
			name:    "nodeport without sub-block",
			expose:  spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort},
			wantErr: true,
		},
		{
			name:    "hostport without sub-block",
			expose:  spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeHostPort},
			wantErr: true,
		},
		{
			name: "loadbalancer with nodePort block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeLoadBalancer,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30565},
			},
			wantErr: true,
		},
		{
			name: "nodeport with hostPort block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30565},
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := testenv.Namespace(t, ctx, c)
			err := c.Create(ctx, proxyGroup(ns, tc.expose))
			if tc.wantErr && err == nil {
				t.Fatal("create succeeded, want CEL rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("create rejected: %v", err)
			}
		})
	}
}

// A live networkRef edit would otherwise leave the existing pods labelled
// with the old network: invisible to ProxyGroupReconciler.pods() and to the
// Service selector, which both derive from the current spec, and never swept
// because their ProxyGroup still exists. They would run forever, holding
// their agent sessions. The CEL rule on ProxyGroupSpec is what rules that out
// before it can happen, and this proves the rule actually rejects the update
// rather than only being present in the schema.
func TestProxyGroupNetworkRefIsImmutable(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	g := proxyGroup(ns, spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30565},
	})
	if err := c.Create(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}

	g.Spec.NetworkRef = spawneryv1alpha1.ObjectRef{Name: "another-network"}
	if err := c.Update(ctx, g); err == nil {
		t.Fatal("update changed spec.networkRef, want rejection")
	}
}
