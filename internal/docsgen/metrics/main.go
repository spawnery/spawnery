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

// Command metrics prints an HTML table (Name, Type, Labels, Help) of every
// spawnery_-prefixed metric registered in controller-runtime's
// metrics.Registry, sorted by name. hack/metrics-docs.sh runs it and embeds
// its output into docs/reference/metrics-and-alerts.md.
//
// It lives under internal/docsgen rather than cmd/: cmd/ holds binaries that
// ship in an image plus spawnery-stubop and spawnery-join, which are
// test-only but still exercised by name from test code. This one is run only
// by hack/metrics-docs.sh, for its stdout, and never imported.
//
// The metrics themselves are never parsed out of Go source. Each package
// that owns metrics (internal/certs, internal/grpcauth, ...) is imported
// here for its init()-time registration side effect, and this program then
// asks the registry itself what it holds -- so the page can't drift from
// what the operator actually exports the way a hand-maintained list could.
package main

import (
	"fmt"
	"html"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	_ "github.com/spawnery/spawnery/internal/agentserver"
	_ "github.com/spawnery/spawnery/internal/certs"
	_ "github.com/spawnery/spawnery/internal/grpcauth"
	_ "github.com/spawnery/spawnery/internal/proxyreg"
	_ "github.com/spawnery/spawnery/internal/rbacaudit"
	_ "github.com/spawnery/spawnery/internal/serverreg"
)

// metricNamePrefix restricts the page to the operator's own metrics.
// Nothing else is registered by the six packages this program imports today,
// but the filter is here on purpose rather than as a defensive no-op: a
// program that merely imports package metrics is exactly the kind of thing a
// later change (a new import pulling in a client-go or controller-runtime
// metrics package as a side effect) could grow an unrelated metric under,
// and this page would then silently start documenting it.
const metricNamePrefix = "spawnery_"

type metricDoc struct {
	Name   string
	Type   string
	Labels []string
	Help   string
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "metrics-docs:", err)
		os.Exit(1)
	}
}

func run(w io.Writer) error {
	descs, err := describeAll()
	if err != nil {
		return err
	}
	observed, err := gatherTypes()
	if err != nil {
		return err
	}
	if err := verifyObservedTypesFitInferType(observed); err != nil {
		return err
	}

	docs := make([]metricDoc, 0, len(descs))
	for name, d := range descs {
		if !strings.HasPrefix(name, metricNamePrefix) {
			continue
		}
		docs = append(docs, metricDoc{Name: name, Type: inferType(name, observed), Labels: d.labels, Help: d.help})
	}
	if len(docs) == 0 {
		return fmt.Errorf("no %s* metric found in the registry -- an import above was dropped, or nothing registers metrics anymore", metricNamePrefix)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })

	_, err = fmt.Fprint(w, renderTable(docs))
	return err
}

// descInfo is what describeAll recovers for one registered metric: its help
// text and its label dimension names. Not its label *values* -- those live
// on samples, which a metric with no sample yet does not have (see
// describeAll).
type descInfo struct {
	help   string
	labels []string
}

// describeAll asks metrics.Registry for the Desc of every collector it holds,
// keyed by metric name.
//
// This is the fix for the trap Gather() has: a GaugeVec or CounterVec with no
// labelled child yet -- true today of, among others, spawnery_ca_rotation_phase
// and spawnery_permissions_missing, neither of which this program's plain
// import-for-side-effects ever sets -- sends nothing through Collect(), so it
// is entirely absent from Gather()'s output. Every registered collector always
// sends its Desc through Describe() regardless, which is why this program asks
// for that instead. Measured directly against this registry: Describe found
// 17 spawnery_ metrics where Gather found 11.
//
// controller-runtime's metrics.Registry is typed as RegistererGatherer, which
// carries neither Describe nor Collect -- both exist on the concrete
// *prometheus.Registry underneath, which is why this asserts to
// prometheus.Collector rather than calling them directly.
func describeAll() (map[string]descInfo, error) {
	collector, ok := metrics.Registry.(prometheus.Collector)
	if !ok {
		return nil, fmt.Errorf("controller-runtime's metrics.Registry (%T) is not a prometheus.Collector", metrics.Registry)
	}

	ch := make(chan *prometheus.Desc)
	done := make(chan error, 1)
	result := map[string]descInfo{}
	go func() {
		for d := range ch {
			name, help, labels, err := parseDesc(d.String())
			if err != nil {
				done <- err
				for range ch {
					// Drain so Describe's send does not block forever.
				}
				return
			}
			if _, dup := result[name]; dup {
				done <- fmt.Errorf("metric %s is registered more than once", name)
				for range ch {
				}
				return
			}
			result[name] = descInfo{help: help, labels: labels}
		}
		done <- nil
	}()
	collector.Describe(ch)
	close(ch)
	if err := <-done; err != nil {
		return nil, err
	}
	return result, nil
}

// parseDesc recovers a Desc's name, help text and variable label names from
// its String() representation -- the only public accessor a *prometheus.Desc
// has, since every one of those fields is otherwise unexported. String()'s
// shape (client_golang v1.23.2's desc.go) is:
//
//	Desc{fqName: %q, help: %q, constLabels: {%s}, variableLabels: {%s}}
//
// fqName and help are Go-quoted (%q) and unquoted with strconv here rather
// than matched by a regexp, so a help string that itself contains a quote or
// a brace cannot be mistaken for the end of the field. None of this
// program's metrics set constLabels, so that section is skipped rather than
// parsed; variableLabels has no metric with more than one label today, but
// the split on "," handles more than one the same way client_golang's own
// String() joins them.
func parseDesc(s string) (name, help string, labels []string, err error) {
	const fqNamePrefix = "Desc{fqName: "
	rest, ok := cutPrefix(s, fqNamePrefix)
	if !ok {
		return "", "", nil, fmt.Errorf("Desc.String() %q does not start with %q", s, fqNamePrefix)
	}
	name, rest, err = quotedField(rest)
	if err != nil {
		return "", "", nil, fmt.Errorf("Desc.String() %q: fqName: %w", s, err)
	}

	const helpPrefix = ", help: "
	rest, ok = cutPrefix(rest, helpPrefix)
	if !ok {
		return "", "", nil, fmt.Errorf("Desc.String() %q has no %q after fqName", s, helpPrefix)
	}
	help, rest, err = quotedField(rest)
	if err != nil {
		return "", "", nil, fmt.Errorf("Desc.String() %q: help: %w", s, err)
	}

	const constLabelsPrefix = ", constLabels: {"
	rest, ok = cutPrefix(rest, constLabelsPrefix)
	if !ok {
		return "", "", nil, fmt.Errorf("Desc.String() %q has no %q after help", s, constLabelsPrefix)
	}
	const variableLabelsMarker = "}, variableLabels: {"
	i := strings.Index(rest, variableLabelsMarker)
	if i < 0 {
		return "", "", nil, fmt.Errorf("Desc.String() %q has no %q", s, variableLabelsMarker)
	}
	rest = rest[i+len(variableLabelsMarker):]
	// Two trailing braces: the variableLabels list's own close, then the
	// outer Desc{...}'s.
	rest = strings.TrimSuffix(rest, "}}")
	if rest != "" {
		labels = strings.Split(rest, ",")
	}
	return name, help, labels, nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

func quotedField(s string) (value, rest string, err error) {
	q, err := strconv.QuotedPrefix(s)
	if err != nil {
		return "", "", fmt.Errorf("no quoted string at start of %q: %w", s, err)
	}
	value, err = strconv.Unquote(q)
	if err != nil {
		return "", "", err
	}
	return value, s[len(q):], nil
}

// gatherTypes reads the real metric type -- gauge, counter, ... -- for every
// metric Gather() has at least one sample for. It is deliberately not the
// source of the metric list: see describeAll for what Gather() misses.
func gatherTypes() (map[string]string, error) {
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		return nil, err
	}
	types := make(map[string]string, len(mfs))
	for _, mf := range mfs {
		types[mf.GetName()] = strings.ToLower(mf.GetType().String())
	}
	return types, nil
}

// verifyObservedTypesFitInferType checks two things about every metric
// Gather() actually has a sample for, both of which inferType depends on to
// name the type of a metric that has *no* sample:
//
//  1. Its type is "counter" or "gauge". inferType has no third case, so a
//     Histogram or Summary that ever picks up a sample before generation
//     runs must fail here rather than fall through inferType's two-way
//     naming-convention check and print as one of them.
//  2. "counter" and "the name ends in _total" agree -- the Prometheus
//     naming convention this codebase's own metrics.go files follow without
//     exception today.
//
// Either one failing means a convention inferType relies on for metrics
// Gather() has no sample for at all has stopped holding, and this is what
// turns that into a loud failure here rather than a wrong type silently
// printed on the page.
func verifyObservedTypesFitInferType(observed map[string]string) error {
	for name, typ := range observed {
		if typ != "counter" && typ != "gauge" {
			return fmt.Errorf(
				"%s is type %s, which inferType has no case for -- it can only tell a metric with no sample apart as counter or gauge, by whether its name ends in _total",
				name, typ,
			)
		}
		endsTotal := strings.HasSuffix(name, "_total")
		isCounter := typ == "counter"
		if endsTotal != isCounter {
			return fmt.Errorf(
				"%s is type %s but %s in _total: the _total naming convention inferType relies on for metrics with no sample yet no longer holds",
				name, typ, map[bool]string{true: "ends", false: "does not end"}[endsTotal],
			)
		}
	}
	return nil
}

// inferType names a metric's type from a sample if Gather() has one, and
// from the _total naming convention (verified above) otherwise. Every metric
// in this registry today is a Gauge or a Counter -- nothing here uses a
// Histogram or a Summary -- so "does not end in _total" defaults to "gauge"
// rather than needing a third case.
//
// The residual this cannot close: a Histogram or Summary added with no
// sample yet at generation time is invisible to Gather() and so invisible
// to verifyObservedTypesFitInferType too, and would still be printed here as
// "gauge" -- Desc carries no type at all, so a metric with no sample
// genuinely cannot be typed from the registry. The check above catches it
// the moment such a metric has a sample when this program runs; until then,
// this comment is the only place that says so.
func inferType(name string, observed map[string]string) string {
	if typ, ok := observed[name]; ok {
		return typ
	}
	if strings.HasSuffix(name, "_total") {
		return "counter"
	}
	return "gauge"
}

func renderTable(docs []metricDoc) string {
	var b strings.Builder
	b.WriteString("<div style=\"overflow-x: auto;\"><table>\n")
	b.WriteString("<thead><tr><th>Name</th><th>Type</th><th>Labels</th><th>Help</th></tr></thead>\n<tbody>\n")
	for _, d := range docs {
		labelCell := "<em>none</em>"
		if len(d.Labels) > 0 {
			parts := make([]string, len(d.Labels))
			for i, l := range d.Labels {
				parts[i] = "<code>" + html.EscapeString(l) + "</code>"
			}
			labelCell = strings.Join(parts, ", ")
		}
		fmt.Fprintf(&b,
			"<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(d.Name), html.EscapeString(d.Type), labelCell, html.EscapeString(d.Help),
		)
	}
	b.WriteString("</tbody></table></div>\n")
	return b.String()
}
