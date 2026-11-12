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

package leaseguard

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newGuard(t *testing.T, holder string) (*Guard, client.Client, *clock) {
	t.Helper()
	return newGuardWith(t, holder, interceptor.Funcs{})
}

func newGuardWith(t *testing.T, holder string, funcs interceptor.Funcs) (*Guard, client.Client, *clock) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "op.example", Namespace: "spawnery-system"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lease).WithInterceptorFuncs(funcs).Build()
	clk := &clock{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	return &Guard{
		Client: c, Namespace: "spawnery-system", Lease: "op.example",
		Hostname: "op-a", Interval: DefaultInterval, Clock: clk.Now,
	}, c, clk
}

func setHolder(t *testing.T, c client.Client, holder string) {
	t.Helper()
	lease := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "spawnery-system", Name: "op.example"}, lease); err != nil {
		t.Fatal(err)
	}
	lease.Spec.HolderIdentity = ptr.To(holder)
	if err := c.Update(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestTheGuardIsFreshAfterReadingItselfAsHolder(t *testing.T) {
	g, _, _ := newGuard(t, "op-a_0529de70")
	if err := g.Confirm(context.Background()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !g.Fresh() {
		t.Error("Fresh() = false right after the Lease named this host")
	}
}

func TestTheGuardIsNotFreshBeforeItsFirstRead(t *testing.T) {
	g, _, _ := newGuard(t, "op-a_0529de70")
	if g.Fresh() {
		t.Error("Fresh() = true before any read of the Lease")
	}
}

func TestTheGuardIsNotFreshWhileAnotherHostHolds(t *testing.T) {
	g, c, _ := newGuard(t, "op-a_0529de70")
	if err := g.Confirm(context.Background()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	setHolder(t, c, "op-b_fc99a90a")
	if err := g.Confirm(context.Background()); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("Confirm: err %v, want ErrNotHolder", err)
	}
	if g.Fresh() {
		t.Error("Fresh() = true although the Lease names another host")
	}
}

func TestAHostnameThatPrefixesTheHoldersIsNotTheHolder(t *testing.T) {
	g, _, _ := newGuard(t, "op-ab_0529de70")
	if err := g.Confirm(context.Background()); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("Confirm: err %v, want ErrNotHolder for op-a against holder op-ab_…", err)
	}
	if g.Fresh() {
		t.Error("Fresh() = true for a host whose name only prefixes the holder's")
	}
}

func TestTheGuardGoesStaleWithoutAConfirmation(t *testing.T) {
	g, _, clk := newGuard(t, "op-a_0529de70")
	if err := g.Confirm(context.Background()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	clk.now = clk.now.Add(3*DefaultInterval - time.Millisecond)
	if !g.Fresh() {
		t.Fatal("Fresh() = false inside three intervals of the last confirmation")
	}
	clk.now = clk.now.Add(time.Millisecond)
	if g.Fresh() {
		t.Error("Fresh() = true three intervals after the last confirmation: a leader frozen " +
			"past its lease would act on its old belief")
	}
}

func refusing(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return err
		},
	}
}

func TestAFailedReadLetsTheConfirmationAgeOut(t *testing.T) {
	g, c, clk := newGuard(t, "op-a_0529de70")
	if err := g.Confirm(context.Background()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	g.Client = interceptor.NewClient(c.(client.WithWatch), refusing(errors.New("connection refused")))
	clk.now = clk.now.Add(DefaultInterval)
	if err := g.Confirm(context.Background()); err == nil || errors.Is(err, ErrNotHolder) {
		t.Fatalf("Confirm: err %v, want a read error", err)
	}
	if !g.Fresh() {
		t.Error("Fresh() = false after one failed read: a slow API server would hold every " +
			"server's next step")
	}
}

func TestANilGuardIsFresh(t *testing.T) {
	var g *Guard
	if !g.Fresh() {
		t.Error("a nil *Guard is not fresh: callers without leader election would never act")
	}
}

func TestStartConfirmsAtOnce(t *testing.T) {
	g, _, _ := newGuard(t, "op-a_0529de70")
	g.Clock = time.Now
	g.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !g.Fresh() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v, want nil", err)
	}
	if !g.Fresh() {
		t.Error("Start did not confirm the Lease before its first tick")
	}
}

func TestTheGuardRunsOnlyOnTheLeader(t *testing.T) {
	if !(&Guard{}).NeedLeaderElection() {
		t.Error("NeedLeaderElection() = false: a standby's guard would read another host's " +
			"Lease and log an error every interval")
	}
}

func wantNamed(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error, want one that stops the manager")
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not name %q", err, p)
		}
	}
}

func TestAnotherHolderStopsTheGuard(t *testing.T) {
	g, _, _ := newGuard(t, "op-b_fc99a90a")
	err := g.fatal(g.Confirm(context.Background()))
	wantNamed(t, err, "spawnery-system", "op.example", `"op-a_"`, `"op-b_fc99a90a"`)
	if !errors.Is(err, ErrNotHolder) {
		t.Errorf("error %v does not wrap ErrNotHolder", err)
	}
}

func TestStartReturnsWhenAnotherHostHolds(t *testing.T) {
	g, _, _ := newGuard(t, "op-b_fc99a90a")
	g.Clock = time.Now
	g.Interval = time.Hour
	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	select {
	case err := <-done:
		wantNamed(t, err, "op.example", `"op-a_"`)
	case <-time.After(5 * time.Second):
		t.Fatal("Start kept running although the Lease names another host: a leader that lost " +
			"its lease or reads the wrong identity would hold every decision back for good")
	}
}

func TestAMissingOrForbiddenLeaseStopsTheGuard(t *testing.T) {
	leases := schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}
	for _, tc := range []struct {
		name  string
		err   error
		names string
	}{
		{"missing", apierrors.NewNotFound(leases, "op.example"), "does not exist"},
		{"forbidden", apierrors.NewForbidden(leases, "op.example", errors.New("no rule")), "leases get"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _ := newGuardWith(t, "op-a_0529de70", refusing(tc.err))
			g.Clock = time.Now
			g.Interval = time.Hour
			done := make(chan error, 1)
			go func() { done <- g.Start(context.Background()) }()
			select {
			case err := <-done:
				wantNamed(t, err, "spawnery-system", "op.example", `"op-a_"`, tc.names)
			case <-time.After(5 * time.Second):
				t.Fatal("Start kept running on its first read: a guard reading the wrong Lease " +
					"or without the Role's leases get would hold every decision back for good")
			}
		})
	}
}

func TestAReadThatKeepsTimingOutNeverStopsTheGuard(t *testing.T) {
	var clk *clock
	var reads atomic.Int32
	g, _, clk := newGuardWith(t, "op-a_0529de70", interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			clk.now = clk.now.Add(DefaultReadTimeout)
			reads.Add(1)
			return context.DeadlineExceeded
		},
	})
	g.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for reads.Load() < 20 && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("Start returned %v after %d timed-out reads: the elector keeps a leader "+
				"through a slow API server, and the guard would crash-loop it", err, reads.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v, want nil", err)
	}
	if n := reads.Load(); n < 20 {
		t.Fatalf("only %d reads in 5s", n)
	}
	if g.Fresh() {
		t.Error("Fresh() = true although no read ever answered")
	}
}

func TestTheConfirmationIsTimedBeforeTheRead(t *testing.T) {
	var clk *clock
	g, _, clk := newGuardWith(t, "op-a_0529de70", interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			clk.now = clk.now.Add(3 * DefaultInterval)
			return err
		},
	})
	if err := g.Confirm(context.Background()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if g.Fresh() {
		t.Error("Fresh() = true after a read whose answer was processed three intervals late: " +
			"a freeze between the API's answer and its processing would count as a confirmation")
	}
}

func TestAReadSlowerThanTheIntervalStillConfirms(t *testing.T) {
	g, _, _ := newGuardWith(t, "op-a_0529de70", interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			select {
			case <-time.After(DefaultInterval + 500*time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := g.Confirm(context.Background()); err != nil {
		t.Errorf("Confirm: %v, want a read of %s to answer: the elector's own requests may "+
			"take %s", err, DefaultInterval+500*time.Millisecond, DefaultReadTimeout)
	}
}

func TestEachReadIsBoundedByTheReadTimeout(t *testing.T) {
	g, _, _ := newGuardWith(t, "op-a_0529de70", interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	g.ReadTimeout = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- g.Confirm(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Confirm: err %v, want a deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hanging Lease read held Confirm past five seconds, and with it every " +
			"later confirmation")
	}
}
