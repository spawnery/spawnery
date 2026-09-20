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

package controller

import (
	"context"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
)

// sweepOnDemand removes the members of an on-demand group whose run is over.
//
// Two phases and two answers. Finished means the server said its round was
// over and then its pod stopped -- a player closing their own world -- and
// there is nothing about it left to keep: the world is on its claim, which
// this operator never deletes, while the object holds the one name its owner
// needs in order to start again. Failed is not swept here: a world that broke
// is one somebody has to be able to look at, and pruneFailed already keeps
// the newest of them and no more. What stops a corpse from blocking a restart
// is the start request, which replaces a terminal member of the key it was
// asked for.
//
// A node that is leaving takes a member with it, and that removal is not this
// one: size() condemns every server on a departing node whatever its type or
// phase, because one left there loses its pod when the node goes and drops its
// players where a condemnation moves them through the proxies first. Nothing
// recreates it and nothing has to -- the world is on its claim, so the key is
// free and its owner starts it again the way they started it the first time.
func (r *ServerGroupReconciler) sweepOnDemand(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
) error {
	for _, v := range views {
		if v.Phase != phase.Finished || v.leaving() {
			continue
		}
		if err := r.deleteServer(ctx, group, servers, v.Name, "InstanceFinished",
			"removing finished on-demand member %s, its world is on its claim"); err != nil {
			return err
		}
	}
	return nil
}
