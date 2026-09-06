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

// NextNumber is the lowest number from 1 upwards that taken does not hold.
//
// Lowest free rather than highest plus one, so a group that scales up and down
// all day keeps its numbers short instead of counting into three digits. The
// cost is that a number is handed out again once its server is gone, which
// ServerInfo.incarnation is what tells apart.
//
// The loop is bounded by len(taken)+1: each iteration that continues needs a
// distinct member of a finite set.
func NextNumber(taken map[int32]bool) int32 {
	for n := int32(1); ; n++ {
		if !taken[n] {
			return n
		}
	}
}

// takenNumbers is the set NextNumber searches: every number a live server of
// the group holds, and every number a create this reconciler issued reserved
// before the cache showed it.
func takenNumbers(views []ServerView, pending map[int32]bool) map[int32]bool {
	taken := make(map[int32]bool, len(views)+len(pending))
	for _, v := range views {
		if v.Number > 0 {
			taken[v.Number] = true
		}
	}
	for n := range pending {
		taken[n] = true
	}
	return taken
}
