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
	"context"
	"net/http/httptest"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/testenv"
)

func TestAReplicaIsReadyOnceItsCachesHaveSynced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		standby bool
	}{{"leader", false}, {"standby", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := testenv.Client(t)
			ns := testenv.Namespace(t, ctx, c)

			const elsewhere = "op-elsewhere"
			if tc.standby {
				now := metav1.NewMicroTime(time.Now())
				lease := &coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{Name: leaderElectionID, Namespace: ns},
					Spec: coordinationv1.LeaseSpec{
						HolderIdentity:       ptr.To(elsewhere),
						LeaseDurationSeconds: ptr.To[int32](3600),
						AcquireTime:          &now,
						RenewTime:            &now,
					},
				}
				if err := c.Create(ctx, lease); err != nil {
					t.Fatalf("create Lease: %v", err)
				}
			}

			mgr, err := ctrl.NewManager(testenv.Config(t), managerOptions(managerFlags{
				leaderElect: true, operatorNamespace: ns, metricsAddr: "0", probeAddr: "0",
			}))
			if err != nil {
				t.Fatalf("new manager: %v", err)
			}
			check := readyzCheck(mgr)
			ready := func() error { return check(httptest.NewRequest("GET", "/readyz", nil)) }

			if err := ready(); err == nil {
				t.Fatal("readyz = nil before the manager started")
			}

			mgrCtx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- mgr.Start(mgrCtx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Error("manager did not stop within 20s")
				}
			})

			within(t, 30*time.Second, "readyz to pass once the caches have synced", func() (bool, string) {
				if err := ready(); err != nil {
					return false, err.Error()
				}
				return true, ""
			})

			holder := func() string {
				var lease coordinationv1.Lease
				if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: leaderElectionID}, &lease); err != nil {
					return err.Error()
				}
				return ptr.Deref(lease.Spec.HolderIdentity, "")
			}
			if tc.standby {
				if h := holder(); h != elsewhere {
					t.Errorf("the Lease is held by %q, want %q: the replica was no standby", h, elsewhere)
				}
				return
			}
			within(t, 20*time.Second, "the replica to hold the Lease", func() (bool, string) {
				h := holder()
				return h != "" && h != elsewhere, "holder " + h
			})
		})
	}
}
