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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/spawnery/spawnery/internal/podspec"
)

// errForeignConfigMap is a sentinel so callers put a condition on the group
// instead of requeueing for ever with nothing on the object.
var errForeignConfigMap = errors.New("a ConfigMap at this group's rendered name is not this group's")

// reconcileGroupConfigMap refuses any object at <group>-<role>-config whose
// controller reference does not carry this group's UID.
// SetControllerReference alone would silently adopt an object with no
// controller; the UID check also refuses a condemned predecessor of the same
// name.
//
// An unlabelled collision is invisible to the narrowed ConfigMap cache and
// surfaces as AlreadyExists on Create; both shapes end at the same sentinel.
// No uncached read tells them apart: it would cost a request per reconcile.
func reconcileGroupConfigMap(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	name string,
	data []byte,
) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace()},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		// UID, not ResourceVersion: the question is the object's identity.
		if cm.UID != "" {
			if controller := metav1.GetControllerOf(cm); controller == nil || controller.UID != owner.GetUID() {
				return fmt.Errorf("%w: %s/%s", errForeignConfigMap, cm.Namespace, cm.Name)
			}
		}
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[podspec.ConfigValuesKey] = string(data)
		return controllerutil.SetControllerReference(owner, cm, scheme)
	})
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("%w: %s/%s", errForeignConfigMap, owner.GetNamespace(), name)
	}
	return err
}

// foreignConfigMapMessage names the transient cause too: a delete-and-recreate
// meets the old group's ConfigMap until the garbage collector removes it, and
// deleting it by hand then would only race the collector.
func foreignConfigMapMessage(namespace, name string) string {
	return fmt.Sprintf(
		"ConfigMap %s/%s exists and is not owned by this group, so this group's configuration cannot be "+
			"written and no pod can start. If this group was just deleted and recreated under the same "+
			"name, the ConfigMap is the old group's and this clears once garbage collection removes it. "+
			"Otherwise delete that ConfigMap or give this group a different name.",
		namespace, name)
}
