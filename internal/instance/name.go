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

// Package instance composes the name of a member of an OnDemand group.
//
// Its own package for the reason internal/boost is one: the operator's request
// endpoint and its controllers both need this rule and must not import each
// other.
package instance

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ErrBadKey is a key that cannot be part of an object's name.
var ErrBadKey = errors.New("that key cannot be part of a server name")

// MaxNameLength is what a Server's name may be, because it is a DNS label and
// the pod that carries it is named from it.
const MaxNameLength = validation.DNS1123LabelMaxLength

// Name composes the member of group that carries key.
//
// Checked here rather than left to the API server: a name that is refused on
// create fails a player's start with whatever the apiserver says about label
// syntax, which is an answer about Kubernetes to a person asking about their
// server. A group whose own name is long is the case that bites -- a
// 36-character UUID key leaves 26 characters for the group -- and the refusal
// says so.
func Name(group, key string) (string, error) {
	if errs := validation.IsDNS1123Label(key); len(errs) > 0 {
		return "", fmt.Errorf("%w: %s", ErrBadKey, errs[0])
	}
	name := group + "-" + key
	if len(name) > MaxNameLength {
		return "", fmt.Errorf("%w: %q is %d characters and a server name may be %d",
			ErrBadKey, name, len(name), MaxNameLength)
	}
	return name, nil
}
