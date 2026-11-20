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
	"errors"
	"log"
	"os"
	"sync/atomic"
	"time"
)

// Default timing parameters for file watching. They are variables rather than
// constants so tests can shrink them.
var (
	// DefaultDebounceInterval is how long the watcher waits for a burst of
	// directory events to settle before reloading.
	DefaultDebounceInterval = 100 * time.Millisecond
	// DefaultStableWindow is how long two consecutive reads must return
	// identical bytes before the content is considered fully written.
	DefaultStableWindow = 50 * time.Millisecond
	// DefaultStableTimeout bounds the total time spent waiting for a writer to
	// finish. On timeout the load fails and the previous snapshot stays
	// active; the next change event (or periodic resync) retries.
	DefaultStableTimeout = 5 * time.Second
	// DefaultResyncInterval is the safety-net periodic re-read that heals a
	// missed filesystem event. Zero disables it.
	DefaultResyncInterval = 60 * time.Second
)

// Option configures an Engine.
type Option func(*Engine)

// WithMetrics attaches a Metrics implementation.
func WithMetrics(m Metrics) Option {
	return func(e *Engine) { e.metrics = m }
}

// WithLogger overrides the standard library logger used for load results.
func WithLogger(l StdLogger) Option {
	return func(e *Engine) { e.logger = l }
}

// WithTiming overrides reload timing, mainly for tests.
func WithTiming(debounce, stableWindow, stableTimeout, resync time.Duration) Option {
	return func(e *Engine) {
		e.debounce = debounce
		e.stableWindow = stableWindow
		e.stableTimeout = stableTimeout
		e.resync = resync
	}
}

// WithReadFunc replaces the file read function. Production code keeps the
// os.ReadFile default; tests use it to script deterministic content changes.
func WithReadFunc(f func(string) ([]byte, error)) Option {
	return func(e *Engine) { e.readFile = f }
}

// StdLogger is the subset of log.Logger the engine needs.
type StdLogger interface {
	Printf(format string, v ...any)
}

// Engine owns the currently active Snapshot and orchestrates hot reloads.
//
// Concurrency contract:
//
//   - Snapshot() is a single atomic pointer load: no locks and no I/O on the
//     request path. Callers take one snapshot per admission request and reuse
//     it for every container lookup, guaranteeing whole-request consistency.
//   - Reloading parses, validates and builds the complete new snapshot off the
//     request path; only after every step succeeds does it swap the pointer.
//     A failed load leaves the old snapshot fully effective.
//   - Old snapshots retained by in-flight requests are reclaimed by the GC.
//     Snapshots are pure in-memory structures with no goroutines or file
//     handles, so there is nothing to release explicitly.
type Engine struct {
	file     string
	snapshot atomic.Pointer[Snapshot]
	version  atomic.Uint64

	metrics       Metrics
	logger        StdLogger
	debounce      time.Duration
	stableWindow  time.Duration
	stableTimeout time.Duration
	resync        time.Duration

	detector Detector
	cancel   context.CancelFunc
	done     chan struct{}

	// readFile is the byte source used by readStable. It is a field rather
	// than a hard os.ReadFile call so tests can script exact content
	// sequences (including a half-written file that happens to parse) without
	// depending on wall-clock scheduling.
	readFile func(string) ([]byte, error)
}

// NewEngine creates an Engine bound to configFile. No file is read until the
// first Load/Reload call; until then Snapshot returns nil and Rewrite is a
// pass-through.
func NewEngine(configFile string, opts ...Option) *Engine {
	e := &Engine{
		file:          configFile,
		metrics:       noopMetrics{},
		logger:        log.Default(),
		debounce:      DefaultDebounceInterval,
		stableWindow:  DefaultStableWindow,
		stableTimeout: DefaultStableTimeout,
		resync:        DefaultResyncInterval,
		readFile:      os.ReadFile,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Snapshot returns the active immutable snapshot, or nil before the first
// successful load.
func (e *Engine) Snapshot() *Snapshot {
	return e.snapshot.Load()
}

// Version returns the active snapshot version, or 0 when none is loaded.
func (e *Engine) Version() uint64 {
	if s := e.Snapshot(); s != nil {
		return s.Version
	}
	return 0
}

// Rewrite rewrites an image reference against the active snapshot. With no
// snapshot loaded the reference is returned unchanged.
func (e *Engine) Rewrite(ref string) (string, bool) {
	return e.Snapshot().Rewrite(ref)
}

// LoadBytes runs the full load pipeline (parse, validate, build, publish) on
// in-memory content. This is the entry point used both by file reloads and by
// ConfigMap-driven updates, which is why it takes bytes rather than a path.
//
// On any failure the previously active snapshot is untouched and the error
// (including the offending rule's location) is logged and counted.
func (e *Engine) LoadBytes(data []byte, source string) (*Snapshot, error) {
	checksum := checksumBytes(data)

	if current := e.Snapshot(); current != nil && current.Checksum == checksum {
		// Unchanged content: nothing to build or swap. This makes repeated
		// notifications from a directory event storm essentially free.
		return current, nil
	}

	rules, err := Parse(data, source)
	if err != nil {
		e.fail("parse", checksum, err)
		return nil, err
	}
	if len(rules) == 0 {
		// An empty file (or one without any rules) is refused rather than
		// silently disabling all rewriting: a mistaken truncation must not
		// turn the proxy off without a signal.
		err := ruleError(Location{File: source}, "configuration contains no rules; refusing to load, so the previous rule set stays active")
		e.fail("empty", checksum, err)
		return nil, err
	}

	nextVersion := e.version.Add(1)
	snap, warnings, err := Build(rules, nextVersion, checksum)
	if err != nil {
		// Version number was consumed; roll it back so successful versions
		// stay dense and the active version gauge is meaningful.
		e.version.Add(^uint64(0))
		e.fail("validate", checksum, err)
		return nil, err
	}

	e.snapshot.Store(snap)
	for _, w := range warnings {
		e.logger.Printf("WARNING rules config %s: %s", source, w)
	}
	e.metrics.LoadSucceeded(snap.Version, checksum, len(rules))
	e.logger.Printf("Loaded rule snapshot version=%d checksum=%s rules=%d from %s: %s",
		snap.Version, checksum, len(rules), source, describeRules(rules))
	return snap, nil
}

// fail logs a load failure with full detail and reports it to metrics.
func (e *Engine) fail(reason, checksum string, err error) {
	e.metrics.LoadFailed(reason, checksum)
	e.logger.Printf("ERROR failed to load rules (reason=%s, checksum=%s): %v; keeping previous snapshot active", reason, checksum, err)
}

// readStable reads the bound config file only after its content has stayed
// identical for the stable window, defending against observing a half-written
// file that happens to be parseable.
func (e *Engine) readStable() ([]byte, error) {
	return readFileStable(e.readFile, e.file, e.stableWindow, e.stableTimeout)
}

// readFileStable keeps reading through readFn until two consecutive reads
// return identical bytes.
//
// Worst case: a writer that keeps the file changing continuously prevents
// stability until stableTimeout elapses; the load then fails with
// ErrUnstableRead, the old snapshot remains active, and a later event or the
// periodic resync retries. A writer that finishes within the timeout is
// observed exactly once its final bytes are durable.
func readFileStable(readFn func(string) ([]byte, error), file string, stableWindow, stableTimeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(stableTimeout)
	first, err := readFn(file)
	if err != nil {
		return nil, err
	}
	for {
		time.Sleep(stableWindow)
		second, err := readFn(file)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(first, second) {
			return second, nil
		}
		if time.Now().After(deadline) {
			return nil, ErrUnstableRead
		}
		first = second
	}
}

// ErrUnstableRead means the file kept changing throughout the stability
// window budget.
var ErrUnstableRead = errors.New("config file did not stabilize within the timeout; load skipped, previous rules remain active")

// Reload re-reads the bound file through stable-read and publishes a new
// snapshot. Failures are contained: callers may log the returned error but
// the engine has already recorded it.
func (e *Engine) Reload() (*Snapshot, error) {
	data, err := e.readStable()
	if err != nil {
		e.fail("read", "", err)
		return nil, err
	}
	return e.LoadBytes(data, e.file)
}

// ReloadFromFile stable-reads an explicit file path once, without changing
// the engine's permanently bound source. It is used for the initial load of a
// file-mounted configuration before the watcher loop starts.
func (e *Engine) ReloadFromFile(path string) (*Snapshot, error) {
	data, err := readFileStable(e.readFile, path, e.stableWindow, e.stableTimeout)
	if err != nil {
		e.fail("read", "", err)
		return nil, err
	}
	return e.LoadBytes(data, path)
}

// AttachFile rebinds the engine's reload source to a real file path so the
// watcher loop's Reload calls read that file.
func (e *Engine) AttachFile(path string) {
	e.file = path
}

// Start attaches a change detector and runs the reload loop until Stop. The
// detector is injectable so tests can drive reloads deterministically with
// ManualDetector; production code passes NewFSNotifyDetector's result.
//
// Coalescing: events arriving during a reload, or in a tight burst, are
// collapsed by the capacity-one detector channel plus a debounce timer. After
// the quiet period the engine reloads once against whatever the file contains
// now, so of three rapid edits only the final content is ever published.
func (e *Engine) Start(ctx context.Context, detector Detector) {
	e.detector = detector
	runCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.done = make(chan struct{})
	go e.loop(runCtx)
}

// loop is the hot-reload event loop.
func (e *Engine) loop(ctx context.Context) {
	defer close(e.done)
	events := e.detector.Events()
	var resync <-chan time.Time
	if e.resync > 0 {
		ticker := time.NewTicker(e.resync)
		defer ticker.Stop()
		resync = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-resync:
			// Safety net for a lost watch event; LoadBytes no-ops on
			// unchanged checksums.
			if _, err := e.Reload(); err != nil {
				e.logger.Printf("ERROR resync reload failed: %v", err)
			}
		case _, ok := <-events:
			if !ok {
				return
			}
			// Debounce: keep resetting the quiet timer while events keep
			// arriving, then perform exactly one reload against the latest
			// file content. This is what collapses a three-edit release into
			// a single load of the final content.
			timer := time.NewTimer(e.debounce)
		quiet:
			for {
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-events:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(e.debounce)
				case <-timer.C:
					break quiet
				}
			}
			if _, err := e.Reload(); err != nil {
				e.logger.Printf("ERROR event reload failed: %v", err)
			}
		}
	}
}

// Stop terminates the reload loop and closes the detector. It is safe to call
// when Start was never invoked.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
		<-e.done
		e.cancel = nil
	}
	if e.detector != nil {
		_ = e.detector.Close()
		e.detector = nil
	}
}
