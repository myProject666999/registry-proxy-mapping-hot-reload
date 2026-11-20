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

// Package rules implements the registry proxy mapping rule engine and its
// hot-reloading machinery.
//
// Matching semantics (see docs/hot-reload.md for the full specification):
//
//   - A rule prefix is host plus zero or more path segments, e.g.
//     "docker.io" or "docker.io/library".
//   - Prefixes are normalized to lower-cased path segments; matching uses
//     longest-prefix-first and prefixes only match at segment boundaries.
//   - Two rules whose normalized prefixes are identical are an unambiguous
//     conflict (when their targets differ) and loading is rejected, so the
//     runtime never has to break a tie.
//   - Rewriting is applied at most once. Every rule target is added to an
//     exemption set so a rewritten result never gets rewritten again.
//     Rewrite cycles are rejected at load time; non-cyclic rewrite chains
//     are reported as warnings.
//   - A Snapshot is immutable: it is built completely before being published
//     and is never mutated afterwards. The Engine swaps snapshots with an
//     atomic pointer, so a request that takes a snapshot once observes the
//     same rules for its whole lifetime.
package rules

import (
	"fmt"
	"strings"
	"time"
)

// Location points at the place a rule (or a configuration problem) came from.
// File is the configuration source name (a file path or a ConfigMap key) and
// Line is the 1-based line number inside that source. A zero Line means the
// location is unknown (for example programmatically constructed rules).
type Location struct {
	File string
	Line int
}

// String returns "file:line" when a line is known, otherwise just the file.
func (l Location) String() string {
	if l.Line > 0 {
		return fmt.Sprintf("%s:%d", l.File, l.Line)
	}
	return l.File
}

// Rule is a single proxy mapping rule.
//
// Source is the image-reference prefix to match ("docker.io/library"). Target
// is the replacement prefix ("docker.linkos.org/library"). SourceSegs and
// TargetSegs are the normalized segment forms used for matching; TargetRaw
// preserves the target exactly as configured and is used when building
// rewritten image references.
type Rule struct {
	Source     string
	Target     string
	SourceSegs []string
	TargetSegs []string
	TargetRaw  string
	Location   Location
}

// summary returns a compact, deterministic single-line representation used in
// logs so operators can confirm which rules became active.
func (r Rule) summary() string {
	return fmt.Sprintf("%s -> %s (%s)", r.Source, r.Target, r.Location)
}

// RuleError is a load-time configuration error that always carries the
// location of the offending rule so multi-user ConfigMap edits can be traced
// back to a concrete line.
type RuleError struct {
	Location Location
	Reason   string
}

// Error implements error.
func (e *RuleError) Error() string {
	if e.Location.Line > 0 || e.Location.File != "" {
		return fmt.Sprintf("%s: %s", e.Location.String(), e.Reason)
	}
	return e.Reason
}

// ruleError builds a RuleError.
func ruleError(loc Location, format string, args ...any) *RuleError {
	return &RuleError{Location: loc, Reason: fmt.Sprintf(format, args...)}
}

// normalizePrefix lower-cases a prefix and splits it into path segments,
// dropping empty segments so leading/trailing/double slashes cannot change
// the meaning. The first segment is the registry host (which may carry a
// port, e.g. "localhost:5000"); colons are not separators.
func normalizePrefix(p string) []string {
	p = strings.ToLower(strings.TrimSpace(p))
	raw := strings.Split(p, "/")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// trieNode is the immutable matching structure. Each node represents one path
// segment. rule is non-nil on terminal nodes of rule sources; exempt marks
// terminal nodes of rule targets (the rewrite-exemption set).
type trieNode struct {
	children map[string]*trieNode
	rule     *Rule
	exempt   bool
}

func newTrieNode() *trieNode {
	return &trieNode{children: map[string]*trieNode{}}
}

// insert walks (creating as needed) the path of segments and returns the
// terminal node.
func (n *trieNode) insert(segs []string) *trieNode {
	cur := n
	for _, seg := range segs {
		next, ok := cur.children[seg]
		if !ok {
			next = newTrieNode()
			cur.children[seg] = next
		}
		cur = next
	}
	return cur
}

// find walks the path of segments and returns the terminal node, or nil if
// the path does not exist.
func (n *trieNode) find(segs []string) *trieNode {
	cur := n
	for _, seg := range segs {
		next, ok := cur.children[seg]
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// Snapshot is an immutable, published set of rules. Once built it is never
// mutated and contains no goroutines, file handles or other resources that
// need explicit release: unreferenced snapshots are reclaimed by the GC.
type Snapshot struct {
	// Version is a monotonically increasing identifier, bumped on every
	// successful load. It is also exported through metrics and logs.
	Version uint64
	// Checksum is the SHA-256 hex digest of the raw configuration bytes the
	// snapshot was built from.
	Checksum string
	// LoadedAt is when the snapshot was published.
	LoadedAt time.Time
	// Rules holds the rules in their configured order and is used for logs
	// and diagnostics; matching uses Root.
	Rules []Rule
	// Root is the matching trie (rules and exemption set).
	Root *trieNode
}

// RuleCount returns the number of rules in the snapshot.
func (s *Snapshot) RuleCount() int {
	if s == nil {
		return 0
	}
	return len(s.Rules)
}
