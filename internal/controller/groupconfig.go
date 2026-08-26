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

// errForeignConfigMap is returned when something else already occupies the
// name a group renders its ConfigMap at. It is a sentinel because the two
// callers turn it into a condition on their group rather than into a bare
// reconcile error: an error alone would requeue for ever with nothing on the
// object saying why, which for a group that cannot start a single pod is the
// worst of the available outcomes.
var errForeignConfigMap = errors.New("a ConfigMap at this group's rendered name is not this group's")

// reconcileGroupConfigMap writes the group's rendered configuration to
// <group>-<role>-config, and refuses to touch an object of that name that
// this group does not own.
//
// # Why the refusal needs writing out
//
// controllerutil.SetControllerReference is not the check it looks like. It
// refuses an object that already has a *different* controller, and silently
// adopts one that has no controller at all -- so a ConfigMap sitting at the
// rendered name, carrying podspec.LabelManagedBy and owned by nobody, used to
// be taken over: its podspec.ConfigValuesKey overwritten with this group's
// document, and an owner reference added that hands it to the garbage
// collector when the group goes. Somebody else's object, rewritten and then
// deleted, with nothing anywhere saying it happened.
//
// The rule applied instead is the one design section 4 states and
// ProxyGroupReconciler.deleteServiceIfOurs already follows for Services: a
// controller reference whose UID is this group's. The UID is the strong half.
// It also refuses this group's own predecessor of the same name, which is
// what a delete-and-recreate can leave standing while the garbage collector
// catches up -- and adopting that would mean writing into an object already
// condemned.
//
// # The two shapes a collision takes
//
// A colliding ConfigMap that carries podspec.LabelManagedBy is visible to the
// manager's cache, so CreateOrUpdate's Get finds it and the mutate closure
// below refuses it.
//
// One *without* the label is invisible: cmd/spawnery-operator narrows the
// ConfigMap cache to that label, so the Get misses, CreateOrUpdate proceeds to
// Create, and the API server answers AlreadyExists. That is the more likely
// collision of the two -- it needs no knowledge of this operator's labels,
// only of its naming -- and until this function it produced a bare error and
// an endless requeue with nothing on the group. Both shapes now end at the
// same sentinel and the same condition, because to a user they are one
// situation: something else is at that name.
//
// There is deliberately no second, uncached read to tell the two apart. It
// would cost a request per reconcile of every group to sharpen a message
// about a state neither shape lets the group run in.
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
		// UID rather than ResourceVersion for "does this already exist". Both
		// are empty on the create path -- CreateOrUpdate's Get failed, so
		// nothing filled either -- but a UID is the object's identity, which
		// is the question being asked.
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

// foreignConfigMapMessage is what the condition says. One function so the two
// group kinds say the same thing about the same situation, and so the remedy
// is stated where a user meets the problem rather than only in this file.
//
// It names the transient cause as well as the standing one, because the two
// need opposite responses and only a person can tell them apart. Deleting a
// group and recreating it under the same name reaches this state legitimately:
// the old ConfigMap is owned by the old group's UID and stands until the
// garbage collector removes it, so the new group refuses an object that is
// already condemned and clears within a requeue or two of its going. Telling
// someone to delete it by hand in that window would have them racing the
// garbage collector for no reason.
func foreignConfigMapMessage(namespace, name string) string {
	return fmt.Sprintf(
		"ConfigMap %s/%s exists and is not owned by this group, so this group's configuration cannot be "+
			"written and no pod can start. If this group was just deleted and recreated under the same "+
			"name, the ConfigMap is the old group's and this clears once garbage collection removes it. "+
			"Otherwise delete that ConfigMap or give this group a different name.",
		namespace, name)
}
