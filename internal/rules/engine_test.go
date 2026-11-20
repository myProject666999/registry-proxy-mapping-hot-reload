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

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// testTimings shrinks the reload timing so tests run fast while keeping the
// same code paths as production.
func testTimings() Option {
	return WithTiming(5*time.Millisecond, 2*time.Millisecond, 200*time.Millisecond, 0)
}

func discardLogger() StdLogger {
	return log.New(&bytes.Buffer{}, "", 0)
}

const (
	configV1 = `
rules:
- source: docker.io
  target: v1.example
`
	configV2 = `
rules:
- source: docker.io
  target: v2.example
`
	configInvalid = `
rules:
- source: docker.io
  target: v-bad.example
- source: docker.io
  target: v-other.example
`
	// Parseable on its own but missing required data: no rules.
	configNoRules = `
enabled: true
`
)

// writeConfig replaces the file content.
func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// eventually polls until cond is true or the deadline passes.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("eventually timed out: " + msg)
}

// TestHotReloadVersionSequence drives the required scenario end to end:
// valid -> invalid -> empty -> incomplete-but-parseable -> valid, asserting
// the active version sequence and that failures keep the old snapshot.
func TestHotReloadVersionSequence(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfg, configV1)

	engine := NewEngine(cfg, testTimings(), WithLogger(discardLogger()))
	detector := NewManualDetector()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx, detector)

	// Initial load.
	if _, err := engine.Reload(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if engine.Version() != 1 {
		t.Fatalf("initial version = %d, want 1", engine.Version())
	}
	if got, _ := engine.Rewrite("docker.io/app:1"); got != "v1.example/app:1" {
		t.Fatalf("v1 rewrite = %q", got)
	}

	// Valid change: version advances to 2.
	writeConfig(t, cfg, configV2)
	detector.Trigger()
	eventually(t, time.Second, func() bool { return engine.Version() == 2 }, "v2 should activate")
	if got, _ := engine.Rewrite("docker.io/app:1"); got != "v2.example/app:1" {
		t.Fatalf("v2 rewrite = %q", got)
	}

	// Invalid content: rejected, version and matching stay on v2.
	writeConfig(t, cfg, configInvalid)
	detector.Trigger()
	time.Sleep(50 * time.Millisecond)
	if engine.Version() != 2 {
		t.Fatalf("version changed after invalid load: %d", engine.Version())
	}
	if got, _ := engine.Rewrite("docker.io/app:1"); got != "v2.example/app:1" {
		t.Fatalf("old rules changed after invalid load: %q", got)
	}

	// Empty file: refused, old snapshot still active.
	writeConfig(t, cfg, "")
	detector.Trigger()
	time.Sleep(50 * time.Millisecond)
	if engine.Version() != 2 {
		t.Fatalf("version changed after empty file: %d", engine.Version())
	}

	// Parseable but rule-less file: refused for the same reason.
	writeConfig(t, cfg, configNoRules)
	detector.Trigger()
	time.Sleep(50 * time.Millisecond)
	if engine.Version() != 2 {
		t.Fatalf("version changed after rule-less file: %d", engine.Version())
	}

	// Valid again: version advances to 3 and new rules apply.
	writeConfig(t, cfg, configV1)
	detector.Trigger()
	eventually(t, time.Second, func() bool { return engine.Version() == 3 }, "v3 should activate")
	if got, _ := engine.Rewrite("docker.io/app:1"); got != "v1.example/app:1" {
		t.Fatalf("v3 rewrite = %q", got)
	}

	engine.Stop()
}

// TestLoadBytesFailureDetails asserts failure errors carry the rule location
// and a specific reason rather than a generic message.
func TestLoadBytesFailureDetails(t *testing.T) {
	engine := NewEngine("memory://config", WithLogger(discardLogger()))
	if _, err := engine.LoadBytes([]byte(configV1), "memory://config"); err != nil {
		t.Fatalf("seed load: %v", err)
	}
	_, err := engine.LoadBytes([]byte(configInvalid), "memory://config")
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "memory://config:3") || !strings.Contains(msg, "conflict") {
		t.Errorf("failure error lacks location/reason: %q", msg)
	}
	if engine.Version() != 1 {
		t.Errorf("version changed after failure: %d", engine.Version())
	}
}

// TestStableRead verifies the half-written-file defense deterministically.
// A scripted read function first keeps returning different parseable content
// (simulating an in-progress write whose partial bytes happen to parse), then
// settles on the final content. The engine must never publish an intermediate
// state and must end with the final rules active.
func TestStableRead(t *testing.T) {
	intermediate := func(i int) []byte {
		return []byte(fmt.Sprintf("rules:\n- source: docker.io\n  target: mid-%d.example\n", i))
	}
	final := []byte(configV2)

	var reads atomic.Int32
	scripted := func(string) ([]byte, error) {
		n := reads.Add(1)
		switch {
		case n <= 4:
			// First four reads keep changing: no two consecutive reads agree.
			return intermediate(int(n)), nil
		default:
			// Writer finished: every later read returns the final bytes.
			return final, nil
		}
	}

	engine := NewEngine("scripted://config",
		WithTiming(5*time.Millisecond, time.Millisecond, time.Second, 0),
		WithReadFunc(scripted), WithLogger(discardLogger()))

	snap, err := engine.Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := snap.Rewrite("docker.io/app:1"); got != "v2.example/app:1" {
		t.Fatalf("published non-final content: %q", got)
	}
	if reads.Load() < 5 {
		t.Errorf("expected multiple reads before stability, got %d", reads.Load())
	}
}

// TestStableReadTimeout checks the worst case: endless churn fails the load
// instead of publishing a half-written configuration, and the previous
// snapshot stays authoritative. The never-ending churn is scripted via the
// injectable read function so scheduling jitter cannot make it flaky.
func TestStableReadTimeout(t *testing.T) {
	seed := []byte(configV1)
	var churn atomic.Int64
	engine := NewEngine("scripted://config",
		WithTiming(5*time.Millisecond, time.Millisecond, 10*time.Millisecond, 0),
		WithReadFunc(func(string) ([]byte, error) {
			// Every read differs: stability is unreachable.
			n := churn.Add(1)
			return []byte(fmt.Sprintf("rules:\n- source: docker.io\n  target: churn-%d.example\n", n)), nil
		}),
		WithLogger(discardLogger()))
	if _, err := engine.LoadBytes(seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := engine.Reload()
	if err == nil {
		t.Fatal("expected unstable-read failure")
	}
	if !strings.Contains(err.Error(), "stabilize") {
		t.Errorf("unexpected error: %v", err)
	}
	// Old snapshot remains authoritative.
	if got, _ := engine.Rewrite("docker.io/app:1"); got != "v1.example/app:1" {
		t.Fatalf("old snapshot lost after unstable read: %q", got)
	}
}

// TestCoalesceRapidChanges fires many triggers with different content; the
// final active content must be the last write, never an intermediate one.
func TestCoalesceRapidChanges(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfg, configV1)
	engine := NewEngine(cfg, testTimings(), WithLogger(discardLogger()))
	detector := NewManualDetector()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx, detector)
	if _, err := engine.Reload(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	final := `
rules:
- source: docker.io
  target: final.example
`
	for i := 0; i < 20; i++ {
		writeConfig(t, cfg, fmt.Sprintf("rules:\n- source: docker.io\n  target: mid-%d.example\n", i))
		detector.Trigger()
	}
	writeConfig(t, cfg, final)
	detector.Trigger()

	eventually(t, time.Second, func() bool {
		got, _ := engine.Rewrite("docker.io/app:1")
		return got == "final.example/app:1"
	}, "final content should win over intermediate writes")

	// No intermediate target may have been the last published result.
	got, _ := engine.Rewrite("docker.io/app:1")
	if strings.Contains(got, "mid-") {
		t.Errorf("intermediate content is active: %q", got)
	}
	engine.Stop()
}

// TestConcurrentReloadAndMatch runs matching workers against an engine that
// keeps switching versions, under the race detector. Each worker simulates a
// single admission request: take one snapshot and use it for many lookups,
// asserting every lookup in the request comes from the same version and the
// two versions always yield distinguishable answers.
func TestConcurrentReloadAndMatch(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfg, configV1)
	engine := NewEngine(cfg, testTimings(), WithLogger(discardLogger()))
	detector := NewManualDetector()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx, detector)
	if _, err := engine.Reload(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const workers = 32
	var failures atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// One "request": one snapshot, many container lookups. All
				// answers must agree on the version that snapshot encodes.
				snap := engine.Snapshot()
				version := snap.Version
				wantTarget := fmt.Sprintf("v%d.example/app:1", version)
				for i := 0; i < 20; i++ {
					got, _ := snap.Rewrite("docker.io/app:1")
					if got != wantTarget {
						failures.Add(1)
						return
					}
				}
			}
		}()
	}

	for v := 2; v <= 8; v++ {
		content := fmt.Sprintf("rules:\n- source: docker.io\n  target: v%d.example\n", v)
		writeConfig(t, cfg, content)
		detector.Trigger()
		eventually(t, time.Second, func() bool { return engine.Version() == uint64(v) },
			fmt.Sprintf("version %d should activate", v))
	}

	close(stop)
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("observed cross-version inconsistency in %d requests", failures.Load())
	}
	engine.Stop()
}

// TestPrometheusMetrics verifies the success/failure counters and the active
// version gauge expose which rule set is in effect.
func TestPrometheusMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics(reg)
	engine := NewEngine("memory://config", WithMetrics(m), WithLogger(discardLogger()))

	if _, err := engine.LoadBytes([]byte(configV1), "memory://config"); err != nil {
		t.Fatalf("load v1: %v", err)
	}
	if _, err := engine.LoadBytes([]byte(configV2), "memory://config"); err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if _, err := engine.LoadBytes([]byte(configInvalid), "memory://config"); err == nil {
		t.Fatal("invalid config should fail")
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	values := map[string]float64{}
	for _, mf := range mfs {
		for _, metric := range mf.GetMetric() {
			switch mf.GetName() {
			case "registry_proxy_rules_version":
				values["version"] = metric.GetGauge().GetValue()
			case "registry_proxy_rules_loaded_total":
				values["rules"] = metric.GetGauge().GetValue()
			case "registry_proxy_rules_reload_success_total":
				values["success"] = metric.GetCounter().GetValue()
			case "registry_proxy_rules_reload_failure_total":
				values["failure:"+metric.GetLabel()[0].GetValue()] = metric.GetCounter().GetValue()
			case "registry_proxy_rules_active_checksum_info":
				values["checksum:"+metric.GetLabel()[0].GetValue()] = metric.GetGauge().GetValue()
			}
		}
	}
	if values["version"] != 2 {
		t.Errorf("active version gauge = %v, want 2", values["version"])
	}
	if values["success"] != 2 {
		t.Errorf("success counter = %v, want 2", values["success"])
	}
	if values["failure:validate"] != 1 {
		t.Errorf("failure counter = %v, want 1", values["failure:validate"])
	}
	var hasChecksum bool
	for k := range values {
		if strings.HasPrefix(k, "checksum:") {
			hasChecksum = true
		}
	}
	if !hasChecksum {
		t.Error("active checksum gauge missing")
	}
}

// TestFSNotifySymlinkSwap reproduces the ConfigMap update pattern: the file
// the engine reads is a symlink, and an update replaces the symlink target
// atomically. The directory watcher must observe the swap.
func TestFSNotifySymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	real1 := filepath.Join(dir, "v1")
	real2 := filepath.Join(dir, "v2")
	writeConfig(t, real1, configV1)
	writeConfig(t, real2, configV2)
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(real1, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	detector, err := NewFSNotifyDetector(link)
	if err != nil {
		t.Fatalf("detector: %v", err)
	}
	defer detector.Close()

	engine := NewEngine(link, testTimings(), WithLogger(discardLogger()))
	if _, err := engine.Reload(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if got, _ := engine.Rewrite("docker.io/app:1"); got != "v1.example/app:1" {
		t.Fatalf("initial rewrite = %q", got)
	}

	// Atomic symlink replacement, mirroring Kubernetes ..data swaps.
	tmp := filepath.Join(dir, "config.tmp")
	if err := os.Symlink(real2, tmp); err != nil {
		t.Fatalf("temp symlink: %v", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatalf("rename swap: %v", err)
	}

	select {
	case <-detector.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("directory watcher did not observe the symlink swap")
	}

	snap, err := engine.Reload()
	if err != nil {
		t.Fatalf("reload after swap: %v", err)
	}
	if got, _ := snap.Rewrite("docker.io/app:1"); got != "v2.example/app:1" {
		t.Fatalf("post-swap rewrite = %q", got)
	}
}

// TestNilSnapshotPassThrough documents pre-load behavior: no rules means no
// rewriting and no panic.
func TestNilSnapshotPassThrough(t *testing.T) {
	engine := NewEngine("memory://config", WithLogger(discardLogger()))
	got, rewritten := engine.Rewrite("docker.io/app:1")
	if rewritten || got != "docker.io/app:1" {
		t.Errorf("nil snapshot behavior = (%q,%v)", got, rewritten)
	}
}
