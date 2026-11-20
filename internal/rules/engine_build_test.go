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
	"strings"
	"testing"
)

// buildForTest compiles a YAML rule document, failing the test on load error.
func buildForTest(t *testing.T, yamlDoc string) *Snapshot {
	t.Helper()
	rules, err := Parse([]byte(yamlDoc), "test.yaml")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	snap, warnings, err := Build(rules, 1, "test-checksum")
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(warnings) > 0 {
		t.Logf("build warnings: %v", warnings)
	}
	return snap
}

// TestLongestPrefix covers the core matching rule: when two rules both match,
// the one with more path segments wins regardless of declaration order.
func TestLongestPrefix(t *testing.T) {
	doc := `
rules:
- source: docker.io
  target: proxy-a.example
- source: docker.io/library
  target: proxy-b.example/library
`
	// Same semantics with declaration order reversed: the outcome must not
	// depend on which rule appears first.
	docReversed := `
rules:
- source: docker.io/library
  target: proxy-b.example/library
- source: docker.io
  target: proxy-a.example
`
	cases := []struct {
		name string
		doc  string
		in   string
		want string
	}{
		{"host-only rule wins for non-library image", doc, "docker.io/ketches/app:v1", "proxy-a.example/ketches/app:v1"},
		{"longer rule wins for library image", doc, "docker.io/library/nginx:1.25", "proxy-b.example/library/nginx:1.25"},
		{"order independence: longer still wins", docReversed, "docker.io/library/nginx:1.25", "proxy-b.example/library/nginx:1.25"},
		{"order independence: host rule still applies elsewhere", docReversed, "docker.io/ketches/app:v1", "proxy-a.example/ketches/app:v1"},
		{"digest suffix is preserved", doc, "docker.io/library/nginx@sha256:abcdef", "proxy-b.example/library/nginx@sha256:abcdef"},
		{"tag and digest are both preserved", doc, "docker.io/ketches/app:tag@sha256:abc", "proxy-a.example/ketches/app:tag@sha256:abc"},
		{"unmatched registry is untouched", doc, "quay.io/ketches/app:v1", "quay.io/ketches/app:v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := buildForTest(t, tc.doc)
			got, _ := snap.Rewrite(tc.in)
			if got != tc.want {
				t.Errorf("Rewrite(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSegmentBoundary proves prefixes only match on segment boundaries:
// docker.io/lib must not match docker.io/library/...
func TestSegmentBoundary(t *testing.T) {
	doc := `
rules:
- source: docker.io/lib
  target: proxy.example/lib
- source: docker.io
  target: proxy.example
`
	cases := []struct {
		in   string
		want string
	}{
		{"docker.io/lib/nginx:1", "proxy.example/lib/nginx:1"},
		// "library" starts with "lib" but is a different segment: the
		// docker.io rule must win, not the docker.io/lib rule.
		{"docker.io/library/nginx:1", "proxy.example/library/nginx:1"},
		{"docker.io/library-ish/x:1", "proxy.example/library-ish/x:1"},
		// Exact segment match still works.
		{"docker.io/lib:1", "proxy.example/lib:1"},
	}
	snap := buildForTest(t, doc)
	for _, tc := range cases {
		got, _ := snap.Rewrite(tc.in)
		if got != tc.want {
			t.Errorf("Rewrite(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDuplicatePrefixConflict checks that same-prefix rules with different
// targets are rejected at load time, with both locations named.
func TestDuplicatePrefixConflict(t *testing.T) {
	doc := `
rules:
- source: docker.io
  target: proxy-a.example
- source: docker.io
  target: proxy-b.example
`
	rules, err := Parse([]byte(doc), "config.yaml")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	_, _, err = Build(rules, 1, "sum")
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"docker.io", "conflict", "config.yaml:3", "config.yaml:5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}

	// Identical duplicate is accepted (redundant but unambiguous).
	dup := `
rules:
- source: docker.io
  target: proxy-a.example
- source: docker.io
  target: proxy-a.example
`
	snap := buildForTest(t, dup)
	if snap.RuleCount() != 1 {
		t.Errorf("expected 1 merged rule, got %d", snap.RuleCount())
	}
}

// TestCaseInsensitiveConflict checks normalization: prefixes differing only
// in case (or slash style) normalize to the same key and conflict.
func TestCaseInsensitiveConflict(t *testing.T) {
	doc := `
rules:
- source: Docker.IO
  target: proxy-a.example
- source: docker.io/
  target: proxy-b.example
`
	rules, err := Parse([]byte(doc), "config.yaml")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	_, _, err = Build(rules, 1, "sum")
	if err == nil {
		t.Fatal("expected case-normalization conflict, got nil")
	}
	if !strings.Contains(err.Error(), "normalization") {
		t.Errorf("error should mention normalization: %v", err)
	}

	// Matching itself is case-insensitive because inputs are normalized.
	snap := buildForTest(t, `
rules:
- source: docker.io/library
  target: proxy.example/library
`)
	got, _ := snap.Rewrite("DOCKER.IO/LIBRARY/Nginx:1")
	if want := "proxy.example/library/nginx:1"; got != want {
		t.Errorf("case-insensitive rewrite = %q, want %q", got, want)
	}
}

// TestTargetHitsOtherRule covers rewrite chains and the single-rewrite
// guarantee, including the idempotence of feeding output back in.
func TestTargetHitsOtherRule(t *testing.T) {
	t.Run("non-cyclic chain warns and rewrites once", func(t *testing.T) {
		doc := `
rules:
- source: docker.io
  target: mirror.example
- source: mirror.example
  target: upstream.example
`
		rules, err := Parse([]byte(doc), "config.yaml")
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		snap, warnings, err := Build(rules, 1, "sum")
		if err != nil {
			t.Fatalf("Build failed: %v", err)
		}
		if len(warnings) == 0 {
			t.Error("expected a rewrite-chain warning")
		}
		out, rewritten := snap.Rewrite("docker.io/library/nginx:1")
		if want := "mirror.example/library/nginx:1"; out != want || !rewritten {
			t.Errorf("first rewrite = (%q,%v), want %q", out, rewritten, want)
		}
		// The rewritten result must be stable under a second application.
		again, rewrittenAgain := snap.Rewrite(out)
		if again != out || rewrittenAgain {
			t.Errorf("second rewrite changed result: %q -> %q (rewritten=%v)", out, again, rewrittenAgain)
		}
		// Direct references to the target host are exempt as well.
		direct, rewritten := snap.Rewrite("mirror.example/foo:1")
		if direct != "mirror.example/foo:1" || rewritten {
			t.Errorf("exempt target got rewritten: %q (%v)", direct, rewritten)
		}
	})

	t.Run("cycle is rejected with rule locations", func(t *testing.T) {
		doc := `
rules:
- source: a.example
  target: b.example
- source: b.example
  target: a.example
`
		rules, err := Parse([]byte(doc), "config.yaml")
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		_, _, err = Build(rules, 1, "sum")
		if err == nil {
			t.Fatal("expected cycle error, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "cycle") || !strings.Contains(msg, "config.yaml:3") {
			t.Errorf("cycle error missing detail: %q", msg)
		}
	})

	t.Run("three-node cycle is rejected", func(t *testing.T) {
		doc := `
rules:
- source: a.example
  target: b.example
- source: b.example
  target: c.example
- source: c.example
  target: a.example
`
		rules, _ := Parse([]byte(doc), "config.yaml")
		if _, _, err := Build(rules, 1, "sum"); err == nil {
			t.Fatal("expected three-node cycle error")
		}
	})
}

// TestParseLegacyProxies ensures the flat map format keeps working and line
// numbers land on the map key.
func TestParseLegacyProxies(t *testing.T) {
	doc := `
enabled: true
proxies:
  docker.io: docker.linkos.org
  quay.io: quay.linkos.org
`
	rules, err := Parse([]byte(doc), "config.yaml")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if rules[0].Source != "docker.io" || rules[0].TargetRaw != "docker.linkos.org" {
		t.Errorf("unexpected first rule: %+v", rules[0])
	}
	if rules[0].Location.Line != 4 {
		t.Errorf("expected line 4, got %d", rules[0].Location.Line)
	}
}

// TestParseValidation covers per-rule load-time validation.
func TestParseValidation(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"empty target", "rules:\n- source: docker.io\n  target: ''\n"},
		{"empty source", "rules:\n- source: ''\n  target: x.example\n"},
		{"scheme in source", "rules:\n- source: https://docker.io\n  target: x.example\n"},
		{"rules not a sequence", "rules: docker.io\n"},
		{"invalid yaml", "rules: [unterminated\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.doc), "config.yaml"); err == nil {
				t.Error("expected parse error, got nil")
			}
		})
	}
}
