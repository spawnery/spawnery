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

package grpcauth_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/podspec"
)

// fakeServerStream embeds the nil interface, so the interceptor panics if it
// touches anything but Context() before authentication succeeds.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func streamCtxWithToken(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

// rejectingReviewer refuses a token; failingReviewer is unreachable.
type rejectingReviewer struct{}

func (rejectingReviewer) Create(context.Context, *authnv1.TokenReview, metav1.CreateOptions) (
	*authnv1.TokenReview, error) {
	return &authnv1.TokenReview{
		Status: authnv1.TokenReviewStatus{Authenticated: false, Error: "bad token"},
	}, nil
}

func TestInterceptorMapsUnavailableToUnavailableCode(t *testing.T) {
	a := &grpcauth.Authenticator{
		Reviews:  failingReviewer{},
		Pods:     refusingPodChecker{},
		Audience: podspec.AgentTokenAudience,
	}
	stream := &fakeServerStream{ctx: streamCtxWithToken("some-token")}

	err := a.StreamInterceptor()(nil, stream,
		&grpc.StreamServerInfo{FullMethod: "/spawnery.agent.v1alpha1.AgentService/ServerSession"},
		func(any, grpc.ServerStream) error {
			t.Fatal("handler ran although the API server was unreachable")
			return nil
		})
	if err == nil {
		t.Fatal("StreamInterceptor accepted the stream although the API server was unreachable")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("code = %v, want %v", code, codes.Unavailable)
	}
}

func TestInterceptorMapsRejectionToUnauthenticatedCode(t *testing.T) {
	a := &grpcauth.Authenticator{
		Reviews:  rejectingReviewer{},
		Pods:     refusingPodChecker{},
		Audience: podspec.AgentTokenAudience,
	}
	stream := &fakeServerStream{ctx: streamCtxWithToken("not-a-real-token")}

	err := a.StreamInterceptor()(nil, stream,
		&grpc.StreamServerInfo{FullMethod: "/spawnery.agent.v1alpha1.AgentService/ServerSession"},
		func(any, grpc.ServerStream) error {
			t.Fatal("handler ran for a token the API server refused")
			return nil
		})
	if err == nil {
		t.Fatal("StreamInterceptor accepted a token the API server refused")
	}
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Errorf("code = %v, want %v", code, codes.Unauthenticated)
	}
}

func TestInterceptorMapsARateLimitToResourceExhausted(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a := &grpcauth.Authenticator{
		Reviews:  rejectingReviewer{},
		Pods:     refusingPodChecker{},
		Audience: podspec.AgentTokenAudience,
		Cache:    grpcauth.NewReviewCache(clock),
		Limiter:  grpcauth.NewPeerLimiter(clock),
	}

	// Distinct tokens all miss the cache; the first PeerBurst come back
	// Unauthenticated from the reviewer, the next is the limiter's refusal.
	for i := 0; i < grpcauth.PeerBurst+2; i++ {
		err := a.StreamInterceptor()(nil,
			&fakeServerStream{ctx: streamCtxWithToken(fmt.Sprintf("distinct-token-%d", i))},
			&grpc.StreamServerInfo{FullMethod: "/spawnery.agent.v1alpha1.AgentService/ServerSession"},
			func(any, grpc.ServerStream) error {
				t.Fatal("handler ran for a token the API server refused")
				return nil
			})
		if err == nil {
			t.Fatalf("call %d was accepted although every token is refused", i)
		}
		switch code := status.Code(err); code {
		case codes.Unauthenticated:
			continue
		case codes.ResourceExhausted:
			return
		default:
			t.Fatalf("call %d came back %v, want Unauthenticated or ResourceExhausted", i, code)
		}
	}
	t.Fatalf("%d calls from one peer and the limiter never engaged", grpcauth.PeerBurst+2)
}
