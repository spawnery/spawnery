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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

func unretireWriter(t *testing.T, srv *spawneryv1alpha1.Server) KubeWriter {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return KubeWriter{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(srv).WithStatusSubresource(srv).Build(),
		Clock:  time.Now,
	}
}

func serverIn(phase string, retire, hold bool) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{Retire: retire, Hold: hold},
		Status:     spawneryv1alpha1.ServerStatus{Phase: phase},
	}
}

func TestUnretireTakesTheRetirementBackAndHoldsTheServer(t *testing.T) {
	w := unretireWriter(t, serverIn("Retiring", true, false))
	if err := w.Unretire(context.Background(), "ns", "lobby-a"); err != nil {
		t.Fatalf("Unretire: %v", err)
	}
	var got spawneryv1alpha1.Server
	if err := w.Client.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "lobby-a"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Retire || !got.Spec.Hold {
		t.Errorf("spec retire=%v hold=%v, want false/true", got.Spec.Retire, got.Spec.Hold)
	}
}

func TestUnretireRefusesWhatIsAlreadyStoppingOrNotRetiring(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *spawneryv1alpha1.Server
		want error
	}{
		{"draining", serverIn("Draining", true, false), ErrServerStopping},
		{"terminating", serverIn("Terminating", true, false), ErrServerStopping},
		{"finished", serverIn("Finished", false, false), ErrServerStopping},
		{"failed", serverIn("Failed", true, false), ErrServerStopping},
		{"never retired", serverIn("Ready", false, false), ErrNotRetiring},
		{"already held", serverIn("Ready", false, true), ErrNotRetiring},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unretireWriter(t, tc.srv).Unretire(context.Background(), "ns", "lobby-a")
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if err := unretireWriter(t, serverIn("Ready", true, false)).Unretire(context.Background(), "ns", "other"); !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("unknown server: err = %v, want ErrNoSuchServer", err)
	}
}

func TestUnretireAnswersEachRefusalForAPerson(t *testing.T) {
	s := &Server{opts: Options{Writer: unretireWriter(t, serverIn("Draining", true, false))}, requestRate: newRequestLimiter(time.Now)}
	resp := s.answerUnretire(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "lobby-b", PodUID: "b", Role: agent.RoleServer},
		3, &agentpb.UnretireRequest{Server: "lobby-a"})
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED || !strings.Contains(resp.GetError().GetMessage(), "already stopping") {
		t.Errorf("answer = %+v, want REFUSED naming that it is already stopping", resp.GetResult())
	}
}
