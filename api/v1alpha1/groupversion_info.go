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

// Package v1alpha1 contains the Spawnery API types.
// +kubebuilder:object:generate=true
// +groupName=spawnery.cloud
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version for all Spawnery types.
	GroupVersion = schema.GroupVersion{Group: "spawnery.cloud", Version: "v1alpha1"}

	// SchemeBuilder registers the Spawnery types with a runtime scheme.
	//
	// scheme.Builder is deprecated: controller-runtime's own note says api
	// packages should carry minimal dependencies and this helper pulls in
	// more than that. The alternative it names is apimachinery's own
	// runtime.SchemeBuilder ([]func(*runtime.Scheme) error) together with a
	// hand-written registration function per type, plus an explicit call to
	// metav1.AddToGroupVersion. Adopting it here would touch all four
	// _types.go files, replacing each file's
	// SchemeBuilder.Register(&T{}, &TList{}) with a closure that calls
	// scheme.AddKnownTypes itself, and would need metav1.AddToGroupVersion
	// wired in by hand where scheme.Builder.Register currently does it for
	// every call. Silenced rather than migrated because that rewrite is a
	// four-file api-package change with no behavioural motivation, out of
	// scope for a lint-cleanup task; it belongs in its own change if this
	// package's dependency footprint ever becomes a real problem.
	//nolint:staticcheck // deprecated scheme.Builder; see above
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the Spawnery types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
