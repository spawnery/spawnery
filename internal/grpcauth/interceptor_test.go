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

package grpcauth_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/podspec"
)

// fakeServerStream is just enough of grpc.ServerStream for the interceptor:
// it only ever calls Context() before a token is accepted. Embedding the
// nil interface would panic if the interceptor called anything else, which
// is the point — it catches the interceptor reaching into the stream before
// authentication succeeds.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func streamCtxWithToken(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

// rejectingReviewer simulates the real API server refusing a token, as
// opposed to failingReviewer, which simulates the API server being
// unreachable. The two must map to different gRPC codes.
type rejectingReviewer struct{}

func (rejectingReviewer) Create(context.Context, *authnv1.TokenReview, metav1.CreateOptions) (
	*authnv1.TokenReview, error) {
	return &authnv1.TokenReview{
		Status: authnv1.TokenReviewStatus{Authenticated: false, Error: "bad token"},
	}, nil
}

// The message-only assertion in TestTokenReviewUnavailableIsNotARejection
// does not prove anything reaches the wire differently: this checks the
// actual gRPC status code the interceptor returns.
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

// A genuinely refused token must still come back as Unauthenticated, not
// Unavailable — otherwise the two codes would carry no information at all.
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
	// The pod checker must never be reached — the review already refused it.
}
