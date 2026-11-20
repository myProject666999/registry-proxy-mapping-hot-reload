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

package cmd

import (
	"testing"

	"github.com/ketches/registry-proxy/internal/rules"
)

// snapshotFromYAML builds an engine snapshot from YAML for webhook-level tests.
func snapshotFromYAML(t *testing.T, doc string) *rules.Snapshot {
	t.Helper()
	parsed, err := rules.Parse([]byte(doc), "webhook-test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	snap, _, err := rules.Build(parsed, 1, "checksum")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return snap
}

func TestGetProxyImage(t *testing.T) {
	doc := `
rules:
- source: docker.io
  target: docker.example
- source: docker.io/library
  target: lib.example/library
`
	snap := snapshotFromYAML(t, doc)

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"implicit docker hub host gets library prefix rule", "nginx:latest", "lib.example/library/nginx:latest"},
		{"explicit library image", "docker.io/library/nginx:1.25", "lib.example/library/nginx:1.25"},
		// image.Parse normalizes bare user images to docker.io/<user>/...,
		// so the host-level rule applies with no library segment.
		{"user image uses host rule", "ketches/app:v1", "docker.example/ketches/app:v1"},
		// A digest-only image is treated as an official image: the parser
		// inserts the library/ segment and the longer rule wins.
		{"digest is preserved (the existing parser drops the tag when a digest is present)",
			"nginx:1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"lib.example/library/nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"unmatched registry untouched", "quay.io/ketches/app:v1", "quay.io/ketches/app:v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := getProxyImage(tc.raw, snap); got != tc.want {
				t.Errorf("getProxyImage(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestGetProxyImageNilSnapshot documents pre-load behavior: without any
// loaded rules every image passes through unchanged and nothing panics.
func TestGetProxyImageNilSnapshot(t *testing.T) {
	raw := "docker.io/library/nginx:1"
	if got := getProxyImage(raw, nil); got != raw {
		t.Errorf("nil snapshot rewrote %q to %q", raw, got)
	}
}

// TestSingleRewriteIdempotence feeds a webhook-level rewrite back through the
// same snapshot and asserts the result is stable (target exemption).
func TestSingleRewriteIdempotence(t *testing.T) {
	doc := `
rules:
- source: docker.io
  target: mirror.example
- source: mirror.example
  target: upstream.example
`
	snap := snapshotFromYAML(t, doc)
	first := getProxyImage("docker.io/ketches/app:1", snap)
	if want := "mirror.example/ketches/app:1"; first != want {
		t.Fatalf("first rewrite = %q, want %q", first, want)
	}
	if second := getProxyImage(first, snap); second != first {
		t.Errorf("second rewrite changed %q to %q; rewritten results must be stable", first, second)
	}
}
