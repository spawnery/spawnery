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

package worldsync

import (
	"fmt"
	"strconv"
	"time"
)

// Retention mirrors v1alpha1.RetentionSpec field for field, so that one
// converts to the other.
type Retention struct {
	Last    int32 `json:"last,omitempty"`
	Hourly  int32 `json:"hourly,omitempty"`
	Daily   int32 `json:"daily,omitempty"`
	Weekly  int32 `json:"weekly,omitempty"`
	Monthly int32 `json:"monthly,omitempty"`
	Yearly  int32 `json:"yearly,omitempty"`
}

// KeepsHistory: a single option of 1 keeps exactly the current generation.
func (r Retention) KeepsHistory() bool {
	return int64(r.Last)+int64(r.Hourly)+int64(r.Daily)+int64(r.Weekly)+int64(r.Monthly)+int64(r.Yearly) > 1
}

type Point struct {
	Generation int64
	Taken      time.Time
}

// Select follows Proxmox Backup Server's mark_selections, one option after
// the other; points are newest first and points[0] is the current
// generation, which is kept whatever the options say.
func (r Retention) Select(points []Point) []bool {
	keep := make([]bool, len(points))
	marked := make([]bool, len(points))
	for _, o := range []struct {
		n  int32
		id func(Point) string
	}{
		{r.Last, func(p Point) string { return strconv.FormatInt(p.Generation, 10) }},
		{r.Hourly, func(p Point) string { return p.Taken.UTC().Format("2006-01-02T15") }},
		{r.Daily, func(p Point) string { return p.Taken.UTC().Format("2006-01-02") }},
		{r.Weekly, func(p Point) string {
			y, w := p.Taken.UTC().ISOWeek()
			return fmt.Sprintf("%d-W%02d", y, w)
		}},
		{r.Monthly, func(p Point) string { return p.Taken.UTC().Format("2006-01") }},
		{r.Yearly, func(p Point) string { return p.Taken.UTC().Format("2006") }},
	} {
		markSelections(points, keep, marked, int(o.n), o.id)
	}
	if len(points) > 0 {
		keep[0] = true
	}
	return keep
}

func markSelections(points []Point, keep, marked []bool, n int, id func(Point) string) {
	covered := map[string]bool{}
	for i, p := range points {
		if keep[i] {
			covered[id(p)] = true
		}
	}
	chosen := map[string]bool{}
	for i, p := range points {
		if marked[i] {
			continue
		}
		sel := id(p)
		if covered[sel] {
			continue
		}
		if chosen[sel] {
			marked[i] = true
			continue
		}
		if len(chosen) >= n {
			return
		}
		chosen[sel] = true
		keep[i], marked[i] = true, true
	}
}
