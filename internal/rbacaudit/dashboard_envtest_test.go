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

package rbacaudit_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/spawnery/spawnery/internal/testenv"
)

func TestTheDashboardRendersOnlyWhenAskedFor(t *testing.T) {
	if _, ok := renderChart(t)["ConfigMap/spawnery-dashboard"]; ok {
		t.Fatal("the dashboard renders by default; it needs a Grafana sidecar nobody asked for")
	}
	doc, ok := renderChartWith(t,
		`{"metrics":{"dashboard":{"enabled":true,"annotations":{"grafana_folder":"Games"}}}}`)["ConfigMap/spawnery-dashboard"]
	if !ok {
		t.Fatal("metrics.dashboard.enabled renders no ConfigMap/spawnery-dashboard")
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(doc, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Labels["grafana_dashboard"] != "1" {
		t.Errorf("labels = %v, want the sidecar's default grafana_dashboard: \"1\"", cm.Labels)
	}
	if cm.Annotations["grafana_folder"] != "Games" {
		t.Errorf("annotations = %v, want the given folder", cm.Annotations)
	}
	var dash map[string]any
	if err := json.Unmarshal([]byte(cm.Data["network.json"]), &dash); err != nil {
		t.Fatalf("network.json is not JSON: %v", err)
	}
	if dash["title"] == nil || dash["panels"] == nil {
		t.Errorf("network.json has no title or panels")
	}
}

// Without honorLabels the Prometheus Operator renames the network's namespace
// label to exported_namespace, and every join with kubelet series is empty.
func TestTheServiceMonitorKeepsTheOperatorsLabels(t *testing.T) {
	doc, ok := renderChartWith(t, `{"metrics":{"serviceMonitor":{"enabled":true}}}`)["ServiceMonitor/spawnery-operator"]
	if !ok {
		t.Fatal("no ServiceMonitor rendered")
	}
	var sm struct {
		Spec struct {
			Endpoints []struct {
				HonorLabels bool `json:"honorLabels"`
			} `json:"endpoints"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(doc, &sm); err != nil {
		t.Fatal(err)
	}
	if len(sm.Spec.Endpoints) == 0 || !sm.Spec.Endpoints[0].HonorLabels {
		t.Fatalf("endpoints = %+v, want honorLabels: true", sm.Spec.Endpoints)
	}
}

// From groups, so a network with every proxy down stays on the dashboard;
// one at a time, because networks usually share group names.
func TestTheDashboardChoosesANetworkFromItsGroups(t *testing.T) {
	raw, err := os.ReadFile(testenv.RepoPath(t, "charts/spawnery/dashboards/network.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		Templating struct {
			List []struct {
				Name       string          `json:"name"`
				Multi      bool            `json:"multi"`
				IncludeAll bool            `json:"includeAll"`
				AllValue   string          `json:"allValue"`
				Query      json.RawMessage `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	for _, v := range dash.Templating.List {
		switch v.Name {
		case "network":
			if !strings.Contains(string(v.Query), "spawnery_group_servers") || v.Multi || v.IncludeAll {
				t.Errorf("network = %+v, want a single choice from spawnery_group_servers", v)
			}
		case "group":
			if v.IncludeAll && v.AllValue != ".*" {
				t.Errorf("group allValue = %q, want .* so All also covers groups without data yet", v.AllValue)
			}
		}
	}
}
