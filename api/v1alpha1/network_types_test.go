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

package v1alpha1

import (
	"testing"

	"k8s.io/utils/ptr"
)

func TestChangeoverBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		net  *Network
		want int32
	}{
		{name: "no update spec", net: &Network{}, want: 0},
		{
			name: "update spec without the field",
			net:  &Network{Spec: NetworkSpec{Update: &NetworkUpdateSpec{}}},
			want: 0,
		},
		{
			name: "explicit budget",
			net:  &Network{Spec: NetworkSpec{Update: &NetworkUpdateSpec{MaxConcurrentChangeovers: ptr.To[int32](2)}}},
			want: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.net.ChangeoverBudget(); got != tc.want {
				t.Errorf("ChangeoverBudget() = %d, want %d", got, tc.want)
			}
		})
	}
}
