// Package observability checks that Jupiter's alerts, the metrics they read and the
// runbooks they name agree.
package observability_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
)

const (
	rulesFile   = "../../deploy/prometheus/alerts.yml"
	runbooks    = "../../docs/runbooks"
	runbookBase = "https://github.com/iricardofernandes/jupiter/blob/main/docs/runbooks/"
)

// instruments are the Prometheus names of the metrics Jupiter records rather than samples,
// as the Collector exports them: counters end in _total, seconds in _seconds, histograms
// in _bucket, _count and _sum.
var instruments = []string{
	// internal/platform/service: background tasks.
	"jupiter_task_runs_total", "jupiter_task_duration_seconds_bucket", "jupiter_task_duration_seconds_count",
	"jupiter_task_duration_seconds_sum", "jupiter_task_last_success", "jupiter_task_interval",
	// internal/api: requests.
	"jupiter_api_request_duration_seconds_bucket", "jupiter_api_request_duration_seconds_count", "jupiter_api_request_duration_seconds_sum",
	// internal/platform/telemetry: database pools.
	"jupiter_db_pool_connections", "jupiter_db_pool_acquires_total", "jupiter_db_pool_waited_acquires_total", "jupiter_db_pool_acquire_wait_seconds_total",
}

type rules struct {
	Groups []struct {
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func known() map[string]bool {
	out := map[string]bool{}
	for _, name := range instruments {
		out[name] = true
	}
	for _, gauges := range [][]telemetry.Gauge{ledger.Gauges, payments.Gauges, api.Gauges, disputes.Gauges, reconciliation.Gauges, jobs.Gauges, acquirer.Gauges} {
		for _, g := range gauges {
			out[strings.ReplaceAll(g.Name, ".", "_")] = true
		}
	}
	return out
}

var metricName = regexp.MustCompile(`\bjupiter_[a-z0-9_]+`)

// Every alert has a severity, a summary and a runbook that exists, reads only metrics
// Jupiter reports, and every runbook is some alert's.
func TestEveryAlertHasARunbookAndKnownMetrics(t *testing.T) {
	raw, err := os.ReadFile(rulesFile)
	if err != nil {
		t.Fatal(err)
	}
	var r rules
	if err := yaml.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	metrics := known()
	named := map[string]bool{}
	for _, g := range r.Groups {
		for _, rule := range g.Rules {
			if sev := rule.Labels["severity"]; sev != "critical" && sev != "warning" {
				t.Errorf("%s: severity %q", rule.Alert, sev)
			}
			if rule.Annotations["summary"] == "" {
				t.Errorf("%s has no summary", rule.Alert)
			}
			file, ok := strings.CutPrefix(rule.Annotations["runbook_url"], runbookBase)
			if !ok || file != rule.Alert+".md" {
				t.Errorf("%s: runbook_url %q, want %s%s.md", rule.Alert, rule.Annotations["runbook_url"], runbookBase, rule.Alert)
			}
			if _, err := os.Stat(filepath.Join(runbooks, file)); err != nil {
				t.Errorf("%s: no runbook: %v", rule.Alert, err)
			}
			named[file] = true
			for _, m := range metricName.FindAllString(rule.Expr, -1) {
				if !metrics[m] {
					t.Errorf("%s reads %s, which Jupiter does not report", rule.Alert, m)
				}
			}
		}
	}
	entries, err := os.ReadDir(runbooks)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "README.md" && !named[e.Name()] {
			t.Errorf("runbook %s belongs to no alert", e.Name())
		}
	}
	readme, err := os.ReadFile(filepath.Join(runbooks, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for file := range named {
		if !strings.Contains(string(readme), "("+file+")") {
			t.Errorf("docs/runbooks/README.md does not list %s", file)
		}
	}
	if len(named) == 0 {
		t.Fatal("no alerts read")
	}
}
