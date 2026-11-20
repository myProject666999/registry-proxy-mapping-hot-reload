/*
Copyright 2024 The Ketches Authors.

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

package rules

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the observability surface of the rule engine. Every load
// attempt is reported exactly once through these callbacks. Implementations
// must be safe for concurrent use.
type Metrics interface {
	// LoadSucceeded is called after a snapshot was built and published.
	// version and checksum identify the active rule set; ruleCount is the
	// number of rules it contains.
	LoadSucceeded(version uint64, checksum string, ruleCount int)
	// LoadFailed is called when a load attempt failed. reason is a short
	// stable category (not the full error text), checksum is empty when the
	// file could not even be read.
	LoadFailed(reason, checksum string)
}

// noopMetrics discards all observations. It is the default when no metrics
// implementation is supplied, keeping the engine usable in plain tests.
type noopMetrics struct{}

func (noopMetrics) LoadSucceeded(uint64, string, int) {}
func (noopMetrics) LoadFailed(string, string)         {}

// PrometheusMetrics adapts the engine to Prometheus. The gauge carrying the
// active version and checksum lets an operator tell exactly which rule set is
// in effect; counters distinguish successful reloads from failures by reason.
type PrometheusMetrics struct {
	activeVersion  prometheus.Gauge
	activeRules    prometheus.Gauge
	loadSuccesses  prometheus.Counter
	loadFailures   *prometheus.CounterVec
	activeChecksum *prometheus.GaugeVec
}

var _ Metrics = (*PrometheusMetrics)(nil)

// NewPrometheusMetrics registers the engine metrics on reg. Pass a dedicated
// registry in tests to avoid duplicate registration; the default registerer
// is used when reg is nil.
func NewPrometheusMetrics(reg prometheus.Registerer) *PrometheusMetrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	m := &PrometheusMetrics{
		activeVersion: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "registry_proxy",
			Name:      "rules_version",
			Help:      "Monotonically increasing version number of the currently active rule snapshot.",
		}),
		activeRules: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "registry_proxy",
			Name:      "rules_loaded_total",
			Help:      "Number of rules in the currently active snapshot.",
		}),
		loadSuccesses: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "registry_proxy",
			Name:      "rules_reload_success_total",
			Help:      "Total number of successful rule reloads.",
		}),
		loadFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "registry_proxy",
			Name:      "rules_reload_failure_total",
			Help:      "Total number of failed rule reloads, labeled by failure reason.",
		}, []string{"reason"}),
		activeChecksum: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "registry_proxy",
			Name:      "rules_active_checksum_info",
			Help:      "Constant 1 gauge carrying the checksum of the active rule set as a label.",
		}, []string{"checksum"}),
	}
	reg.MustRegister(m.activeVersion, m.activeRules, m.loadSuccesses, m.loadFailures, m.activeChecksum)
	return m
}

// LoadSucceeded implements Metrics.
func (m *PrometheusMetrics) LoadSucceeded(version uint64, checksum string, ruleCount int) {
	m.activeVersion.Set(float64(version))
	m.activeRules.Set(float64(ruleCount))
	m.activeChecksum.WithLabelValues(checksum).Set(1)
	m.loadSuccesses.Inc()
}

// LoadFailed implements Metrics.
func (m *PrometheusMetrics) LoadFailed(reason, _ string) {
	m.loadFailures.WithLabelValues(reason).Inc()
}
