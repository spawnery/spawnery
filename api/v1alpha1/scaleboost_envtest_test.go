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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The CRD is installable and the fields survive a write and a read. Cheap, and
// it catches a marker that does not mean what it looks like -- a validation
// that rejects a legal value, or an optional field the API server drops.
func TestScaleBoostRoundTrip(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	expires := metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second))

	boost := &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-boost", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Replicas:  2,
			ExpiresAt: &expires,
		},
	}
	if err := c.Create(ctx, boost); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := &spawneryv1alpha1.ScaleBoost{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-boost", Namespace: ns}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.GroupRef.Name != "lobby" || got.Spec.Replicas != 2 {
		t.Errorf("spec = %+v, want lobby and 2", got.Spec)
	}
	if got.Spec.ExpiresAt == nil || !got.Spec.ExpiresAt.Time.Equal(expires.Time) {
		t.Errorf("expiresAt = %v, want %v", got.Spec.ExpiresAt, expires)
	}
}

func TestAScaleBoostWithoutAnExpiryIsAccepted(t *testing.T) {
	// The "forever" case the type deliberately allows. If this fails, the
	// +optional marker is not doing what the comment says it does.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	if err := c.Create(ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "forever", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Replicas: 1,
		},
	}); err != nil {
		t.Fatalf("a boost with no expiry was refused: %v", err)
	}
}

func TestAScaleBoostOfZeroReplicasIsRefused(t *testing.T) {
	// Minimum=1. A boost of zero is not a boost, and accepting one would put
	// an object in the cluster that inflates nothing and explains nothing.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	if err := c.Create(ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Replicas: 0,
		},
	}); err == nil {
		t.Error("a boost of zero replicas was accepted")
	}
}
