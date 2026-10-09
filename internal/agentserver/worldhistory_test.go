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

package agentserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/worldsync"
)

var historyCaller = grpcauth.Identity{Namespace: "minecraft", PodName: "gateway-0", PodUID: "proxy-history", Role: agent.RoleProxy}

type fakeHistory struct {
	points   []worldsync.RestorePoint
	restored worldsync.Restored
	err      error
	asked    []int64
	policy   worldsync.Retention
	ctxErr   error
	deadline bool
	gate     chan struct{}
	entered  chan struct{}

	listDeadline time.Time
}

func (f *fakeHistory) RestorePoints(ctx context.Context, _ string) ([]worldsync.RestorePoint, error) {
	f.listDeadline, _ = ctx.Deadline()
	return f.points, f.err
}

func (f *fakeHistory) Restore(ctx context.Context, _ string, generation int64, policy worldsync.Retention) (worldsync.Restored, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.ctxErr = ctx.Err()
	_, f.deadline = ctx.Deadline()
	f.asked = append(f.asked, generation)
	f.policy = policy
	return f.restored, f.err
}

func historyServer(t *testing.T, worlds *fakeWorlds, history *fakeHistory, objs ...client.Object) (*Server, *events.FakeRecorder) {
	t.Helper()
	c := fakeClient(t, objs...)
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now}
	if worlds != nil {
		w.Worlds = worlds
	}
	if history != nil {
		w.History = history
	}
	rec := events.NewFakeRecorder(16)
	return &Server{opts: Options{Writer: w, State: netstate.Source{Reader: c}, Recorder: rec}, requestRate: newRequestLimiter(time.Now)}, rec
}

func askRestore(s *Server, generation int64) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), historyCaller, &agentpb.CloudRequest{
		Id: 7, Request: &agentpb.CloudRequest_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldRequest{
			Group: "private-servers", Key: "c0ffee", Generation: generation,
		}},
	})
}

func askRestorePoints(s *Server) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), historyCaller, &agentpb.CloudRequest{
		Id: 8, Request: &agentpb.CloudRequest_ListRestorePoints{ListRestorePoints: &agentpb.ListRestorePointsRequest{
			Group: "private-servers", Key: "c0ffee",
		}},
	})
}

func historyMember(p phase.Phase, stopping bool) *spawneryv1alpha1.Server {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-c0ffee", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"}, Key: "c0ffee"},
		Status:     spawneryv1alpha1.ServerStatus{Phase: string(p)},
	}
	if stopping {
		now := metav1.Now()
		srv.Finalizers = []string{"spawnery.cloud/test"}
		srv.DeletionTimestamp = &now
	}
	return srv
}

func claimGroup() *spawneryv1alpha1.ServerGroup {
	g := objectStoreGroup()
	g.Spec.Storage = &spawneryv1alpha1.StorageSpec{Keep: []string{"world"}}
	return g
}

func TestRestoreWorldAnswersEveryReasonOfTheAPI(t *testing.T) {
	store := []client.Object{objectStoreGroup()}
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		worlds  *fakeWorlds
		history *fakeHistory
		want    agentpb.RequestError_Reason
		result  string
	}{
		{"no such group", nil, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_NOT_FOUND, "refused"},
		{"a group on claims", []client.Object{claimGroup()}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_REFUSED, "refused"},
		{"world sync is off", store, nil, nil, agentpb.RequestError_REFUSED, "refused"},
		{"the member runs", []client.Object{objectStoreGroup(), historyMember(phase.Ready, false)}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_REFUSED, "refused"},
		{"the member is stopping", []client.Object{objectStoreGroup(), historyMember(phase.Ready, true)}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_UNAVAILABLE, "unavailable"},
		{"the world is being deleted", store, &fakeWorlds{pending: true}, &fakeHistory{}, agentpb.RequestError_REFUSED, "refused"},
		{"a node holds the lease", store, &fakeWorlds{}, &fakeHistory{err: &worldsync.HeldError{Node: "node-a"}}, agentpb.RequestError_UNAVAILABLE, "unavailable"},
		{"another restore committed first", store, &fakeWorlds{}, &fakeHistory{err: fmt.Errorf("%w: 412", worldsync.ErrConflict)}, agentpb.RequestError_UNAVAILABLE, "unavailable"},
		{"the key has no world", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrNotFound}, agentpb.RequestError_NOT_FOUND, "refused"},
		{"no such generation", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrNoGeneration}, agentpb.RequestError_NOT_FOUND, "refused"},
		{"the current generation", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrCurrentGeneration}, agentpb.RequestError_REFUSED, "refused"},
		{"the store fails", store, &fakeWorlds{}, &fakeHistory{err: errors.New("store down")}, agentpb.RequestError_UNAVAILABLE, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := historyServer(t, tc.worlds, tc.history, tc.objs...)
			before := testutil.ToFloat64(WorldRestores.WithLabelValues(tc.result))
			resp := askRestore(s, 3)
			if got := resp.GetError().GetReason(); got != tc.want {
				t.Fatalf("reason = %v (%q), want %v", got, resp.GetError().GetMessage(), tc.want)
			}
			if got := testutil.ToFloat64(WorldRestores.WithLabelValues(tc.result)); got != before+1 {
				t.Fatalf("restores{result=%q} = %v, want %v", tc.result, got, before+1)
			}
		})
	}
}

func TestARestoreAnswersTheNewGenerationAndIsRecordedOnTheGroup(t *testing.T) {
	g := objectStoreGroup()
	g.Spec.Storage.Retention = &spawneryv1alpha1.RetentionSpec{Last: 12, Daily: 7}
	history := &fakeHistory{restored: worldsync.Restored{Generation: 9, RestoredFrom: 3, RestoredTaken: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)}}
	s, rec := historyServer(t, &fakeWorlds{}, history, g, historyMember(phase.Failed, false))
	before := testutil.ToFloat64(WorldRestores.WithLabelValues("restored"))

	got := askRestore(s, 3).GetRestoreWorld()
	if got == nil || got.GetGeneration() != 9 || got.GetRestoredFrom() != 3 {
		t.Fatalf("result = %+v, want generation 9 from 3", got)
	}
	if len(history.asked) != 1 || history.asked[0] != 3 || history.policy != (worldsync.Retention{Last: 12, Daily: 7}) {
		t.Fatalf("asked %v with %+v; want generation 3 under the group's retention", history.asked, history.policy)
	}
	if after := testutil.ToFloat64(WorldRestores.WithLabelValues("restored")); after != before+1 {
		t.Fatalf("restores{result=restored} = %v, want %v", after, before+1)
	}
	select {
	case ev := <-rec.Events:
		for _, want := range []string{"WorldRestored", "world c0ffee restored to generation 3 of 2026-10-08T10:00:00Z"} {
			if !strings.Contains(ev, want) {
				t.Errorf("event %q does not say %q", ev, want)
			}
		}
	default:
		t.Fatal("no event was recorded")
	}
}

func TestRestorePointsAreAnsweredNewestFirstWithTheirTimes(t *testing.T) {
	taken := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	history := &fakeHistory{points: []worldsync.RestorePoint{
		{Generation: 9, Taken: taken, Current: true},
		{Generation: 7, Taken: taken.Add(-time.Hour)},
	}}
	s, _ := historyServer(t, &fakeWorlds{}, history, objectStoreGroup(), historyMember(phase.Ready, false))
	points := askRestorePoints(s).GetListRestorePoints().GetPoints()
	if len(points) != 2 ||
		points[0].GetGeneration() != 9 || !points[0].GetCurrent() || points[0].GetTakenUnixMillis() != taken.UnixMilli() ||
		points[1].GetGeneration() != 7 || points[1].GetCurrent() || points[1].GetTakenUnixMillis() != taken.Add(-time.Hour).UnixMilli() {
		t.Fatalf("points = %v; a running member's restore points are listed too", points)
	}
}

func TestAListingOfRestorePointsHasATimeout(t *testing.T) {
	history := &fakeHistory{points: []worldsync.RestorePoint{{Generation: 1, Current: true}}}
	s, _ := historyServer(t, &fakeWorlds{}, history, objectStoreGroup())
	asked := time.Now()
	askRestorePoints(s)
	if history.listDeadline.IsZero() || history.listDeadline.After(asked.Add(restoreTimeout+time.Second)) {
		t.Fatalf("listing deadline = %v; a bucket that never answers would hold the request for the session's life", history.listDeadline)
	}
}

func TestRestorePointsRefusals(t *testing.T) {
	store := []client.Object{objectStoreGroup()}
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		worlds  *fakeWorlds
		history *fakeHistory
		want    agentpb.RequestError_Reason
	}{
		{"no such group", nil, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_NOT_FOUND},
		{"a group on claims", []client.Object{claimGroup()}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_REFUSED},
		{"world sync is off", store, nil, nil, agentpb.RequestError_REFUSED},
		{"the key has no world", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrNotFound}, agentpb.RequestError_NOT_FOUND},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := historyServer(t, tc.worlds, tc.history, tc.objs...)
			if got := askRestorePoints(s).GetError().GetReason(); got != tc.want {
				t.Fatalf("reason = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestARestoreOutlivesThePluginsRequest(t *testing.T) {
	history := &fakeHistory{restored: worldsync.Restored{Generation: 9, RestoredFrom: 3}}
	s, _ := historyServer(t, &fakeWorlds{}, history, objectStoreGroup())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp := s.answerCloudRequest(ctx, logr.Discard(), historyCaller, &agentpb.CloudRequest{
		Id: 7, Request: &agentpb.CloudRequest_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldRequest{
			Group: "private-servers", Key: "c0ffee", Generation: 3,
		}},
	})
	if resp.GetRestoreWorld() == nil {
		t.Fatalf("answer = %v, want a restore", resp)
	}
	if history.ctxErr != nil || !history.deadline {
		t.Fatalf("restore ran on ctx err %v, deadline %v; want a live context with its own timeout", history.ctxErr, history.deadline)
	}
}

func TestABadKeyIsRefusedByBothVerbs(t *testing.T) {
	s, _ := historyServer(t, &fakeWorlds{}, &fakeHistory{}, objectStoreGroup())
	for name, req := range map[string]*agentpb.CloudRequest{
		"restore": {Id: 1, Request: &agentpb.CloudRequest_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldRequest{Group: "private-servers", Key: "Not A Key", Generation: 3}}},
		"list":    {Id: 2, Request: &agentpb.CloudRequest_ListRestorePoints{ListRestorePoints: &agentpb.ListRestorePointsRequest{Group: "private-servers", Key: "Not A Key"}}},
	} {
		if got := s.answerCloudRequest(context.Background(), logr.Discard(), historyCaller, req).GetError().GetReason(); got != agentpb.RequestError_REFUSED {
			t.Errorf("%s: reason = %v, want REFUSED", name, got)
		}
	}
}

func TestAWorldMarkedForDeletionStillListsItsPoints(t *testing.T) {
	history := &fakeHistory{points: []worldsync.RestorePoint{{Generation: 9, Current: true}}}
	s, _ := historyServer(t, &fakeWorlds{pending: true}, history, objectStoreGroup())
	if got := askRestorePoints(s).GetListRestorePoints().GetPoints(); len(got) != 1 {
		t.Fatalf("points = %v, want the one generation", got)
	}
}

func slowRestoreFixture(t *testing.T) (*Server, *fakeHistory) {
	t.Helper()
	history := &fakeHistory{
		restored: worldsync.Restored{Generation: 9, RestoredFrom: 3},
		gate:     make(chan struct{}),
		entered:  make(chan struct{}, 1),
	}
	s, _ := historyServer(t, &fakeWorlds{}, history, objectStoreGroup())
	return s, history
}

func waitEntered(t *testing.T, history *fakeHistory) {
	t.Helper()
	select {
	case <-history.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore never started")
	}
}

func TestAProxyAnswersOtherRequestsWhileARestoreRuns(t *testing.T) {
	s, history := slowRestoreFixture(t)
	late := make(chan *agentpb.OperatorToProxy, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := func(req *agentpb.CloudRequest) *agentpb.ProxyMessage {
		return &agentpb.ProxyMessage{Message: &agentpb.ProxyMessage_CloudRequest{CloudRequest: req}}
	}
	done := make(chan *agentpb.OperatorToProxy, 1)
	go func() {
		done <- s.handleProxy(ctx, logr.Discard(), historyCaller, msg(&agentpb.CloudRequest{
			Id: 7, Request: &agentpb.CloudRequest_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldRequest{Group: "private-servers", Key: "c0ffee", Generation: 3}},
		}), late)
	}()
	select {
	case answer := <-done:
		if answer != nil {
			t.Fatalf("the restore was answered inline: %v", answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleProxy is held by the restore")
	}
	waitEntered(t, history)
	other := s.handleProxy(ctx, logr.Discard(), historyCaller, msg(&agentpb.CloudRequest{Id: 8}), late)
	if other.GetCloudResponse().GetId() != 8 {
		t.Fatalf("another request was not answered meanwhile: %v", other)
	}
	close(history.gate)
	select {
	case answer := <-late:
		if got := answer.GetCloudResponse(); got.GetId() != 7 || got.GetRestoreWorld().GetGeneration() != 9 {
			t.Fatalf("late answer = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the restore's answer never arrived")
	}
}

func TestAServerAnswersOtherRequestsWhileARestoreRuns(t *testing.T) {
	s, history := slowRestoreFixture(t)
	late := make(chan *agentpb.OperatorToServer, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := func(req *agentpb.CloudRequest) *agentpb.ServerMessage {
		return &agentpb.ServerMessage{Message: &agentpb.ServerMessage_CloudRequest{CloudRequest: req}}
	}
	id := historyCaller
	id.Role = agent.RoleServer
	done := make(chan *agentpb.OperatorToServer, 1)
	go func() {
		done <- s.handle(ctx, logr.Discard(), id, msg(&agentpb.CloudRequest{
			Id: 7, Request: &agentpb.CloudRequest_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldRequest{Group: "private-servers", Key: "c0ffee", Generation: 3}},
		}), late)
	}()
	select {
	case answer := <-done:
		if answer != nil {
			t.Fatalf("the restore was answered inline: %v", answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handle is held by the restore")
	}
	waitEntered(t, history)
	other := s.handle(ctx, logr.Discard(), id, msg(&agentpb.CloudRequest{Id: 8}), late)
	if other.GetCloudResponse().GetId() != 8 {
		t.Fatalf("another request was not answered meanwhile: %v", other)
	}
	close(history.gate)
	select {
	case answer := <-late:
		if got := answer.GetCloudResponse(); got.GetId() != 7 || got.GetRestoreWorld().GetGeneration() != 9 {
			t.Fatalf("late answer = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the restore's answer never arrived")
	}
}
