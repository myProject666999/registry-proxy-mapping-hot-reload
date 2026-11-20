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
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"time"
)

// Build validates a parsed rule set and compiles an immutable Snapshot.
//
// Validation performed here, all at load time so the matching path never has
// to resolve ambiguity:
//
//  1. Duplicate normalized prefixes with different targets are a hard error
//     (this also covers prefixes that only differ in case or slash style).
//     Exact duplicates (same source and same target) are harmless and merged.
//  2. Every rule target is inserted into the rewrite-exemption trie so that
//     an already-rewritten reference is never rewritten a second time.
//  3. The rule graph (source -> target) is checked for cycles, which are hard
//     errors. A target that matches another rule's source without forming a
//     cycle is a rewrite chain: it is reported as a warning because single
//     rewrite semantics make the second rule ineffective for traffic entering
//     through the first.
func Build(rules []Rule, version uint64, checksum string) (*Snapshot, []string, error) {
	root := newTrieNode()
	effective := make([]Rule, 0, len(rules))
	warnings := make([]string, 0)

	for i := range rules {
		r := rules[i]
		node := root.insert(r.SourceSegs)
		if node.rule != nil {
			existing := node.rule
			if strings.EqualFold(existing.TargetRaw, r.TargetRaw) {
				// Identical mapping declared twice: keep the first, skip the
				// duplicate. No ambiguity is possible.
				continue
			}
			return nil, nil, ruleError(r.Location,
				"duplicate rule prefix %q conflicts with rule at %s (targets %q vs %q); prefixes are compared after normalization, so case and slash differences count as duplicates",
				r.Source, existing.Location.String(), r.TargetRaw, existing.TargetRaw)
		}
		node.rule = &r
		effective = append(effective, r)
	}

	// Exemption set: all rule targets are exempt from further rewriting.
	for i := range effective {
		root.insert(effective[i].TargetSegs).exempt = true
	}

	// Cycle detection with iterative DFS and coloring. Edges point from a
	// rule source to the rule whose source is a prefix of the target. Only
	// exact target-as-source edges can form cycles, but chain warnings also
	// cover longer prefix matches.
	if err := detectCycles(root, effective); err != nil {
		return nil, nil, err
	}

	// Chain warnings: a target matched by another (different) rule means that
	// other rule would rewrite again under a naive chaining implementation.
	for i := range effective {
		r := effective[i]
		match, _ := lookup(root, r.TargetSegs)
		if match != nil && !samePrefix(match.SourceSegs, r.SourceSegs) {
			warnings = append(warnings,
				"rule at "+r.Location.String()+
					" has target "+r.Target+" that matches another rule ("+match.Source+
					" at "+match.Location.String()+"); only a single rewrite is applied, so the matched rule will not run again for traffic rewritten by the first")
		}
	}

	snap := &Snapshot{
		Version:  version,
		Checksum: checksum,
		LoadedAt: time.Now(),
		Rules:    append([]Rule(nil), effective...),
		Root:     root,
	}
	return snap, warnings, nil
}

// DFS node colors for cycle detection.
const (
	colorWhite = 0
	colorGray  = 1
	colorBlack = 2
)

// dfsFrame is one frame of the iterative DFS.
type dfsFrame struct {
	rule *Rule
}

// detectCycles walks the source->target graph and rejects any back edge. A
// cycle means rewriting could never settle on a stable result, so such a
// configuration is refused at load time with the rules involved. Only exact
// target-to-source edges form graph edges (a target with extra path tail is
// still a rewrite chain, reported separately as a warning).
func detectCycles(root *trieNode, rules []Rule) error {
	color := make(map[string]int, len(rules))

	for start := range rules {
		startRule := &rules[start]
		startKey := strings.Join(startRule.SourceSegs, "/")
		if color[startKey] != colorWhite {
			continue
		}
		color[startKey] = colorGray
		stack := []*dfsFrame{{rule: startRule}}
		// pathIndex maps a gray rule's key to its frame position, so the
		// error message can name every rule on the cycle.
		pathIndex := map[string]int{startKey: 0}

		for len(stack) > 0 {
			top := stack[len(stack)-1]
			next := ruleBySource(root, top.rule.TargetSegs)
			if next == nil {
				markBlack(stack, color, pathIndex)
				stack = stack[:0]
				break
			}
			nextKey := strings.Join(next.SourceSegs, "/")
			switch color[nextKey] {
			case colorGray:
				cycleStart := pathIndex[nextKey]
				names := make([]string, 0, len(stack)-cycleStart+1)
				for _, f := range stack[cycleStart:] {
					names = append(names, f.rule.Source+" ("+f.rule.Location.String()+")")
				}
				names = append(names, next.Source+" ("+next.Location.String()+")")
				return ruleError(top.rule.Location,
					"rewrite cycle detected involving rules: %s; rewrite chains are not followed, remove the cycle",
					strings.Join(names, " -> "))
			case colorBlack:
				// Reachable subtree already proven acyclic.
				markBlack(stack, color, pathIndex)
				stack = stack[:0]
			default:
				color[nextKey] = colorGray
				pathIndex[nextKey] = len(stack)
				stack = append(stack, &dfsFrame{rule: next})
			}
		}
	}
	return nil
}

// markBlack marks every frame in the DFS stack as fully visited.
func markBlack(stack []*dfsFrame, color map[string]int, pathIndex map[string]int) {
	for _, f := range stack {
		key := strings.Join(f.rule.SourceSegs, "/")
		color[key] = colorBlack
		delete(pathIndex, key)
	}
}

// ruleBySource returns the rule whose source equals segs exactly, or nil.
func ruleBySource(root *trieNode, segs []string) *Rule {
	node := root.find(segs)
	if node == nil || node.rule == nil {
		return nil
	}
	return node.rule
}

// samePrefix reports whether two segment slices are equal.
func samePrefix(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lookup implements longest-prefix matching with segment boundaries. It
// returns the rule of the longest matching rule terminal (or nil) and whether
// the walked path hit an exemption terminal. The exemption check is evaluated
// on the full input first by the caller via LookupExempt; here we track the
// deepest exemption node seen during the walk.
func lookup(root *trieNode, segs []string) (*Rule, bool) {
	cur := root
	var best *Rule
	exempt := false
	for _, seg := range segs {
		next, ok := cur.children[seg]
		if !ok {
			break
		}
		cur = next
		if cur.exempt {
			exempt = true
		}
		if cur.rule != nil {
			best = cur.rule
		}
	}
	return best, exempt
}

// Lookup returns the longest-prefix matching rule for a normalized reference,
// or nil when nothing matches. Inputs already starting at an exempted prefix
// (i.e. produced by a previous rewrite) never match.
func (s *Snapshot) Lookup(segs []string) *Rule {
	if s == nil {
		return nil
	}
	rule, exempt := lookup(s.Root, segs)
	if exempt {
		return nil
	}
	return rule
}

// IsExempt reports whether segs starts at a target prefix, meaning the
// reference must not be rewritten again.
func (s *Snapshot) IsExempt(segs []string) bool {
	if s == nil {
		return false
	}
	_, exempt := lookup(s.Root, segs)
	return exempt
}

// splitSuffix separates a trailing ":tag" or "@digest" suffix (or both, in
// either order) from the path segments of an image reference. It returns the
// lower-cased normalized segments and the verbatim suffix to re-attach.
func splitSuffix(ref string) ([]string, string) {
	// Digests never contain "/" so scanning the last segment is sufficient.
	lastSlash := strings.LastIndex(ref, "/")
	last := ref[lastSlash+1:]
	suffix := ""
	head := last
	if at := strings.Index(last, "@"); at >= 0 {
		suffix = last[at:]
		head = last[:at]
	}
	if colon := strings.Index(head, ":"); colon >= 0 {
		suffix = head[colon:] + suffix
		head = head[:colon]
	}
	prefix := ref
	if lastSlash >= 0 {
		prefix = ref[:lastSlash+1] + head
	} else {
		prefix = head
	}
	return normalizePrefix(prefix), suffix
}

// Rewrite applies the snapshot's rules to a single image reference and
// returns the result plus whether any rule fired. Rewriting happens at most
// once: targets are exempt, so feeding the output back in is a no-op. A nil
// snapshot (nothing loaded yet) leaves the reference unchanged.
func (s *Snapshot) Rewrite(ref string) (string, bool) {
	if s == nil {
		return ref, false
	}
	segs, suffix := splitSuffix(ref)
	rule := s.Lookup(segs)
	if rule == nil {
		return ref, false
	}
	// Replace the matched leading segments with the target, keeping the
	// unmatched tail. Matching is prefix-based at segment boundaries, so the
	// tail is always whole segments.
	tail := segs[len(rule.SourceSegs):]
	joined := path.Join(append(append([]string{}, rule.TargetSegs...), tail...)...)
	return joined + suffix, true
}

// checksumBytes returns the hex SHA-256 of data, used as a content version.
func checksumBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
