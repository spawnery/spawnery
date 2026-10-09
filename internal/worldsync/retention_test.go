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
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// pointsAt numbers the times from newest to oldest: the first gets the
// highest generation.
func pointsAt(t *testing.T, times ...string) []Point {
	t.Helper()
	ps := make([]Point, len(times))
	for i, s := range times {
		ps[i] = Point{Generation: int64(len(times) - i), Taken: at(t, s)}
	}
	return ps
}

func keptGenerations(ps []Point, keep []bool) []int64 {
	var out []int64
	for i, k := range keep {
		if k {
			out = append(out, ps[i].Generation)
		}
	}
	return out
}

func TestRetentionSelect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Retention
		times  []string
		want   []int64
	}{
		{
			name:   "without options only the current stays",
			policy: Retention{},
			times:  []string{"2026-10-08T10:50:00Z", "2026-10-08T10:45:00Z", "2026-10-07T09:00:00Z"},
			want:   []int64{3},
		},
		{
			name:   "the current counts as one of last",
			policy: Retention{Last: 2},
			times:  []string{"2026-10-08T10:50:00Z", "2026-10-08T10:45:00Z", "2026-10-08T10:40:00Z", "2026-10-08T10:35:00Z"},
			want:   []int64{4, 3},
		},
		{
			name:   "the newest of each hour, and empty hours do not count",
			policy: Retention{Hourly: 3},
			times: []string{
				"2026-10-08T10:50:00Z", "2026-10-08T10:20:00Z",
				"2026-10-08T09:55:00Z", "2026-10-08T09:05:00Z",
				"2026-10-08T07:30:00Z", "2026-10-08T06:10:00Z",
			},
			want: []int64{6, 4, 2},
		},
		{
			name:   "a tie within one period goes to the newer generation",
			policy: Retention{Hourly: 2},
			times:  []string{"2026-10-08T10:30:00Z", "2026-10-08T10:30:00Z", "2026-10-08T09:00:00Z"},
			want:   []int64{3, 1},
		},
		{
			// The PBS documentation's wording would keep generation 3 as the
			// newest of 2026-10-08 among those older than 4; its code skips
			// the whole day, because last kept 4 from it.
			name:   "a later option skips a period an earlier one kept from",
			policy: Retention{Last: 1, Daily: 2},
			times:  []string{"2026-10-08T10:50:00Z", "2026-10-08T09:50:00Z", "2026-10-08T08:50:00Z", "2026-10-07T23:50:00Z"},
			want:   []int64{4, 1},
		},
		{
			// ISO week 14 of 2026 runs from Monday 30 March to 5 April.
			name:   "the passed-over points of a week across two months stay passed over",
			policy: Retention{Weekly: 1, Monthly: 2},
			times:  []string{"2026-04-02T12:00:00Z", "2026-03-31T12:00:00Z", "2026-03-20T12:00:00Z", "2026-02-10T12:00:00Z"},
			want:   []int64{4, 2, 1},
		},
		{
			// All three fall on 2026-10-07 in UTC; in UTC+2 the first is on the 8th.
			name:   "periods are UTC",
			policy: Retention{Daily: 2},
			times:  []string{"2026-10-08T00:30:00+02:00", "2026-10-07T23:30:00+02:00", "2026-10-07T12:00:00Z"},
			want:   []int64{3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := pointsAt(t, tc.times...)
			got := keptGenerations(ps, tc.policy.Select(ps))
			if len(got) != len(tc.want) {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("kept %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The PBS documentation's example for ten years of daily backups:
// keep-last 3, keep-daily 13, keep-weekly 8, keep-monthly 11, keep-yearly 9.
// Twelve years of points give every option all the periods it asks for.
func TestTheProxmoxDocumentationsTenYearExampleKeeps44(t *testing.T) {
	end := at(t, "2026-10-08T02:00:00Z")
	n := 12 * 366
	ps := make([]Point, n)
	for i := range ps {
		ps[i] = Point{Generation: int64(n - i), Taken: end.AddDate(0, 0, -i)}
	}
	keep := Retention{Last: 3, Daily: 13, Weekly: 8, Monthly: 11, Yearly: 9}.Select(ps)
	if got := len(keptGenerations(ps, keep)); got != 44 {
		t.Fatalf("kept %d, want 3+13+8+11+9 = 44", got)
	}
}

func TestSelectOfNothingIsNothing(t *testing.T) {
	if got := (Retention{Last: 3}).Select(nil); len(got) != 0 {
		t.Fatalf("Select(nil) = %v", got)
	}
}

func TestKeepsHistory(t *testing.T) {
	for _, tc := range []struct {
		r    Retention
		want bool
	}{
		{Retention{}, false},
		{Retention{Last: 1}, false},
		{Retention{Weekly: 1}, false},
		{Retention{Last: 2}, true},
		{Retention{Last: 1, Hourly: 1}, true},
		{Retention{Yearly: 3}, true},
	} {
		if got := tc.r.KeepsHistory(); got != tc.want {
			t.Errorf("%+v.KeepsHistory() = %v, want %v", tc.r, got, tc.want)
		}
	}
}
