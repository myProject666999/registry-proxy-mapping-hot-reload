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
	"context"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// Detector reports that the rule source may have changed. It is an interface
// on purpose: production uses the fsnotify-based watcher, while tests inject
// ManualDetector for deterministic, event-free reloads.
type Detector interface {
	// Events returns a channel that receives a value whenever a reload should
	// be attempted. Multiple rapid changes may be coalesced into fewer
	// notifications; the Engine re-reads current content on every tick.
	Events() <-chan struct{}
	// Close releases any underlying resources.
	Close() error
}

// ManualDetector is a deterministic Detector for tests. Trigger is safe for
// concurrent use; notifications while the Engine is busy coalesce because the
// channel has capacity one, which also exercises the engine's own coalescing.
type ManualDetector struct {
	ch chan struct{}
}

// NewManualDetector creates a ManualDetector.
func NewManualDetector() *ManualDetector {
	return &ManualDetector{ch: make(chan struct{}, 1)}
}

// Trigger asks the Engine to reload. It never blocks: if a previous trigger
// has not yet been consumed, this call is folded into it.
func (m *ManualDetector) Trigger() {
	select {
	case m.ch <- struct{}{}:
	default:
	}
}

// Events implements Detector.
func (m *ManualDetector) Events() <-chan struct{} { return m.ch }

// Close implements Detector.
func (m *ManualDetector) Close() error { return nil }

// fsnotifyDetector watches the *directory* containing the config file rather
// than the file itself.
//
// Why this covers ConfigMap mounts: a ConfigMap volume is updated by replacing
// a set of symlinked directories atomically (..data moves to point at a new
// timestamped directory; the config file is a symlink through ..data).
// Watching only the file's content misses the swap because the inode behind
// the path changes, while watching the parent directory reliably produces
// CREATE/REMOVE/RENAME events for the symlink swap. A plain in-place write
// produces a WRITE event and is covered as well. The intermediate event bursts
// that directory watching introduces are harmless: the Engine debounces and
// re-reads whatever the path currently points at, and stable-read protects
// against catching a half-written file.
type fsnotifyDetector struct {
	watcher *fsnotify.Watcher
	ch      chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	target  string
}

// NewFSNotifyDetector starts a directory watcher for the file at targetPath.
// The returned detector must eventually be Closed.
func NewFSNotifyDetector(targetPath string) (Detector, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(targetPath)
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &fsnotifyDetector{
		watcher: watcher,
		ch:      make(chan struct{}, 1),
		cancel:  cancel,
		done:    make(chan struct{}),
		target:  targetPath,
	}
	go d.loop(ctx)
	return d, nil
}

// loop translates raw fsnotify events into coalesced reload notifications.
func (d *fsnotifyDetector) loop(ctx context.Context) {
	defer close(d.done)
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
			// A watcher error may mean events were lost (e.g. queue overflow).
			// Re-reading unconditionally is the safe response: the content
			// checksum makes a no-op reload cheap.
			_ = err
			d.notify()
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			if d.eventConcernsTarget(ev) {
				d.notify()
			}
		}
	}
}

// eventConcernsTarget reports whether the event could have changed the file
// the engine reads (the file itself, its parent entries such as ..data, or
// the symlink chain). Watching the directory means every event names an entry
// in that directory; all are treated as potentially relevant and filtered
// later by content checksum.
func (d *fsnotifyDetector) eventConcernsTarget(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) == 0 {
		return false
	}
	dir := filepath.Dir(d.target)
	return filepath.Dir(ev.Name) == dir
}

// notify is non-blocking: pending notifications coalesce in the capacity-one
// channel, matching the Engine's "reload to latest content" contract.
func (d *fsnotifyDetector) notify() {
	select {
	case d.ch <- struct{}{}:
	default:
	}
}

// Events implements Detector.
func (d *fsnotifyDetector) Events() <-chan struct{} { return d.ch }

// Close stops the watcher and waits for the translation loop to finish.
func (d *fsnotifyDetector) Close() error {
	d.cancel()
	err := d.watcher.Close()
	<-d.done
	return err
}
