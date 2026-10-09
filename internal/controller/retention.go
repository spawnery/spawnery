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

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/worldsync"
)

// RetentionPublisher puts a group's world retention where the node agents
// read it; the zero policy removes it.
type RetentionPublisher interface {
	Sync(ctx context.Context, namespace, group string, r worldsync.Retention) error
}

func (r *ServerGroupReconciler) publishRetention(ctx context.Context, namespace, group string, policy worldsync.Retention) {
	if r.Retention == nil {
		return
	}
	if err := r.Retention.Sync(ctx, namespace, group, policy); err != nil {
		log.FromContext(ctx).Error(err, "could not publish the group's world retention; the next pass tries again", "group", group)
	}
}
