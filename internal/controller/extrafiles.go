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
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// checkExtraFiles decides whether the claim a group's spec.extraFiles names
// can be served, exactly as checkExtraPlugins does for its own field.
//
// A second flag rather than a wider one: --allow-plugin-volumes exists so an
// operator can say "this installation runs no third-party plugins" and have it
// be a fact, and making it also govern files would leave its name covering
// something that is not a plugin. Like that one, this switch is an operational
// statement and not a security boundary -- a PersistentVolumeClaim is a
// namespaced object in the same trust domain as the group naming it.
func checkExtraFiles(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ef *spawneryv1alpha1.ExtraFiles,
	allowed bool,
) (string, string, bool) {
	if ef == nil {
		return "", "", true
	}
	if !allowed {
		return spawneryv1alpha1.ReasonFileVolumesDisabled,
			"spec.extraFiles is set, and this operator was started without " +
				"--allow-file-volumes so it renders no file volume",
			false
	}

	if problem, ok := checkClaimMountable(ctx, reader, namespace, ef.ClaimName); !ok {
		return spawneryv1alpha1.ReasonFileVolumeUnusable,
			fmt.Sprintf("spec.extraFiles names claim %q, which %s", ef.ClaimName, problem),
			false
	}
	return "", "", true
}
