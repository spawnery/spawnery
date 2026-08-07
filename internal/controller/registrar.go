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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// Registrar is how the Server controller reaches the proxies. Milestone 3
// implements it on top of the gRPC broadcast; milestone 1 wires the no-op, so
// the state machine can be exercised end to end without a proxy.
type Registrar interface {
	// Register tells every connected proxy to accept players for this server.
	Register(ctx context.Context, srv *spawneryv1alpha1.Server) error
	// Deregister tells every proxy to stop sending players here.
	Deregister(ctx context.Context, srv *spawneryv1alpha1.Server) error
	// Drain tells every proxy to move the players off this server.
	Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error
}

// NoopRegistrar logs what it would have sent.
type NoopRegistrar struct{}

// Register implements Registrar.
func (NoopRegistrar) Register(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	log.FromContext(ctx).V(1).Info("would register server with the proxies", "server", srv.Name)
	return nil
}

// Deregister implements Registrar.
func (NoopRegistrar) Deregister(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	log.FromContext(ctx).V(1).Info("would deregister server from the proxies", "server", srv.Name)
	return nil
}

// Drain implements Registrar.
func (NoopRegistrar) Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	log.FromContext(ctx).V(1).Info("would drain players off the server", "server", srv.Name)
	return nil
}
