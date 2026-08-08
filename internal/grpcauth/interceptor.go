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

package grpcauth

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/agent"
)

type identityKey struct{}

// IdentityFrom reads the identity a stream was authenticated with.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// RoleForMethod maps a gRPC method to the role its caller must have.
func RoleForMethod(fullMethod string) (agent.Role, bool) {
	switch {
	case strings.HasSuffix(fullMethod, "/ServerSession"):
		return agent.RoleServer, true
	case strings.HasSuffix(fullMethod, "/ProxySession"):
		return agent.RoleProxy, true
	}
	return "", false
}

// wrappedStream carries the authenticated context into the handler.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w wrappedStream) Context() context.Context { return w.ctx }

// StreamInterceptor authenticates every stream before the handler sees it.
func (a *Authenticator) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		role, ok := RoleForMethod(info.FullMethod)
		if !ok {
			return status.Errorf(codes.Unimplemented, "unknown method")
		}

		ctx := ss.Context()
		id, err := a.Authenticate(ctx, bearerFrom(ctx), role)
		if err != nil {
			// The token itself never reaches the log.
			log.FromContext(ctx).V(1).Info("rejected an agent stream",
				"method", info.FullMethod, "reason", err.Error())
			AuthFailures.WithLabelValues(string(role)).Inc()
			return status.Error(codes.Unauthenticated, err.Error())
		}

		return handler(srv, wrappedStream{ServerStream: ss, ctx: context.WithValue(ctx, identityKey{}, id)})
	}
}

func bearerFrom(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, value := range md.Get("authorization") {
		if token, found := strings.CutPrefix(value, "Bearer "); found {
			return token
		}
	}
	return ""
}
