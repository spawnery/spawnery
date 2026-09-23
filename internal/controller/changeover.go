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
	"sort"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// ChangeoverView is what AdmitChangeovers needs of one group to decide
// whether it may change over.
type ChangeoverView struct {
	Kind    string // "ServerGroup" or "ProxyGroup"
	Name    string
	State   spawneryv1alpha1.ChangeoverState
	Failing bool
}

func changeoverKey(kind, name string) string { return kind + "/" + name }

// AdmitChangeovers returns the groups, keyed "Kind/Name", that may change over
// now. budget < 1 means no cap.
func AdmitChangeovers(groups []ChangeoverView, budget int32) map[string]bool {
	admitted := map[string]bool{}
	var waiting []ChangeoverView
	var holders int32
	for _, g := range groups {
		if g.State == spawneryv1alpha1.ChangeoverNone || g.Failing {
			continue
		}
		if budget < 1 || g.State == spawneryv1alpha1.ChangeoverBegun {
			admitted[changeoverKey(g.Kind, g.Name)] = true
			if g.State == spawneryv1alpha1.ChangeoverBegun {
				holders++
			}
			continue
		}
		waiting = append(waiting, g)
	}
	if budget < 1 {
		return admitted
	}
	sort.Slice(waiting, func(i, j int) bool {
		if waiting[i].Name != waiting[j].Name {
			return waiting[i].Name < waiting[j].Name
		}
		return waiting[i].Kind < waiting[j].Kind
	})
	for _, g := range waiting {
		if holders >= budget {
			break
		}
		admitted[changeoverKey(g.Kind, g.Name)] = true
		holders++
	}
	return admitted
}
