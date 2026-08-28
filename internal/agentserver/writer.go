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

package agentserver

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// ErrNoSuchServer is what a ClusterWriter reports for a server that is not
// there.
//
// A sentinel rather than the API machinery's own not-found, so that the
// interface below stays free of Kubernetes: a reader of ClusterWriter should
// be able to see the whole of what a request can do without knowing what
// backs it.
var ErrNoSuchServer = errors.New("no such server")

// ClusterWriter is every change a plugin's request is allowed to make.
//
// Deliberately one method wide, in the shape ProxyFleet uses and for the same
// reason: this endpoint is the one place in the operator where an instruction
// arrives from inside a game server, and the list of things such an
// instruction can reach should be readable in one screen. A client.Client
// here would instead put every object in the cluster one line away from a
// request handler, and nobody auditing this later could bound it by reading.
type ClusterWriter interface {
	// Retire asks one server to stop taking joins and empty out.
	//
	// It reports whether this call is the one that set the flag. False means
	// somebody had already asked, which the caller answers as REFUSED rather
	// than patching an identical value a second time.
	//
	// It returns ErrNoSuchServer when the server is gone, which is ordinary:
	// the caller resolved the name against a snapshot that is allowed to be a
	// moment stale.
	Retire(ctx context.Context, namespace, name string) (bool, error)
}

// KubeWriter is the ClusterWriter the operator runs with.
type KubeWriter struct {
	// Client must be able to patch servers. The manager's client is cached
	// for reads, which is what makes the already-retiring check below cheap.
	Client client.Client
}

// Retire sets spec.retire on one server.
//
// The read before the patch is not an optimisation. spec.retire is a bool, so
// a blind patch would succeed identically whether or not it changed anything,
// and this verb has to be able to tell the two apart: the operator's answer is
// the only way the person who typed the command learns whether they were the
// one who retired the server or the second person to ask.
//
// The gap between the read and the patch is a race two admins could lose
// together, and losing it costs nothing: the second patch writes the value the
// first one already wrote, and the second admin is told they did something
// they merely repeated. That is a strictly better failure than serialising
// every retire through a conflict-retry loop for a flag that only ever goes
// one way.
func (w KubeWriter) Retire(ctx context.Context, namespace, name string) (bool, error) {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ErrNoSuchServer
		}
		return false, err
	}
	if srv.Spec.Retire {
		return false, nil
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire = true
	if err := w.Client.Patch(ctx, &srv, patch); err != nil {
		return false, err
	}
	return true, nil
}
