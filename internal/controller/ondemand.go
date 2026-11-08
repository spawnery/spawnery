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

// sweepFinishedMembers removes the Finished members whose world is kept: every
// member of an OnDemand group, whose world is on its claim or in the object
// store, and the resumed members of an Ephemeral group, which its sizing rule
// does not see. The name is then free for a restart. Failed members stay for
// inspection (pruneFailed bounds them), and a start request replaces a
// terminal member of its key.
func (r *ServerGroupReconciler) sweepFinishedMembers(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
) error {
	message := "removing finished on-demand member %s, its world is on its claim"
	switch {
	case group.IsEphemeral():
		message = "removing finished resumed member %s, its world is kept in the object store"
	case group.UsesObjectStore():
		message = "removing finished on-demand member %s, its world is kept in the object store"
	}
	for _, v := range views {
		if v.Phase != phase.Finished || v.leaving() {
			continue
		}
		if group.IsEphemeral() && !v.Resumed {
			continue
		}
		if err := r.deleteServer(ctx, group, servers, v.Name, "InstanceFinished", message); err != nil {
			return err
		}
	}
	return nil
}
