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

// Package leaderonly keeps metrics that only leader-bound code writes off the
// scrape of a standby replica, where they would read as zero.
package leaderonly

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

var elected atomic.Bool

// Elect opens the gate for every wrapped collector in the process.
func Elect() { elected.Store(true) }

// Watch calls Elect once elected closes.
func Watch(elected <-chan struct{}) {
	go func() {
		<-elected
		Elect()
	}()
}

type collector []prometheus.Collector

// Wrap describes cs always, so registration and the metrics reference see
// them, but collects them only after Elect.
func Wrap(cs ...prometheus.Collector) prometheus.Collector { return collector(cs) }

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	for _, x := range c {
		x.Describe(ch)
	}
}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	if !elected.Load() {
		return
	}
	for _, x := range c {
		x.Collect(ch)
	}
}
