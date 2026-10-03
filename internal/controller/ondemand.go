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

// sweepOnDemand removes Finished members only: the world is on its claim and
// the name is free for a restart. Failed members stay for inspection
// (pruneFailed bounds them), and a start request replaces a terminal member of
// its key.
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
