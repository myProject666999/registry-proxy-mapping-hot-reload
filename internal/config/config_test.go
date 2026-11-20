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

package config

import (
	"testing"
)

// TestResetLoadsRules verifies the ConfigMap-driven path feeds the rule
// engine and rewriting reflects the loaded proxies/rules.
func TestResetLoadsRules(t *testing.T) {
	doc := []byte(`
enabled: true
proxies:
  docker.io: docker.example
  quay.io: quay.example
excludeNamespaces:
- kube-system
includeNamespaces:
- "*"
podSelector: {}
namespaceSelector: {}
`)
	Reset(doc)
	snap := Engine().Snapshot()
	if snap == nil {
		t.Fatal("expected a snapshot after Reset")
	}
	if snap.RuleCount() != 2 {
		t.Fatalf("expected 2 rules, got %d", snap.RuleCount())
	}
	if got, _ := Engine().Rewrite("docker.io/library/nginx:1"); got != "docker.example/library/nginx:1" {
		t.Errorf("rewrite = %q", got)
	}
}

// TestResetExplicitRulesAndPrefixWins verifies the explicit rules format is
// accepted and longest-prefix semantics apply end to end through config.
func TestResetExplicitRulesAndPrefixWins(t *testing.T) {
	doc := []byte(`
enabled: true
rules:
- source: docker.io
  target: proxy-a.example
- source: docker.io/library
  target: proxy-b.example/library
`)
	Reset(doc)
	cases := map[string]string{
		"docker.io/library/nginx:1": "proxy-b.example/library/nginx:1",
		"docker.io/ketches/app:1":   "proxy-a.example/ketches/app:1",
		"gcr.io/x:1":                "gcr.io/x:1",
	}
	for in, want := range cases {
		got, _ := Engine().Rewrite(in)
		if got != want {
			t.Errorf("Rewrite(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResetInvalidRulesKeepsOldSnapshot ensures a bad ConfigMap update cannot
// disable rewriting: the previously active snapshot stays in effect.
func TestResetInvalidRulesKeepsOldSnapshot(t *testing.T) {
	good := []byte(`
proxies:
  docker.io: good.example
`)
	Reset(good)
	goodVersion := Engine().Version()

	bad := []byte(`
rules:
- source: docker.io
  target: bad-a.example
- source: DOCKER.IO
  target: bad-b.example
`)
	Reset(bad)

	if Engine().Version() != goodVersion {
		t.Errorf("version advanced after invalid load: %d -> %d", goodVersion, Engine().Version())
	}
	got, _ := Engine().Rewrite("docker.io/app:1")
	if got != "good.example/app:1" {
		t.Errorf("old rules not retained: %q", got)
	}
}

// TestResetEmptyRestoresDefaults covers the ConfigMap deletion path.
func TestResetEmptyRestoresDefaults(t *testing.T) {
	Reset(nil)
	if !Enabled() {
		t.Error("expected enabled default config")
	}
	got, _ := Engine().Rewrite("docker.io/app:1")
	if got != "docker.linkos.org/app:1" {
		t.Errorf("default rules not restored: %q", got)
	}
}

// TestSelectorAccessors exercise the RLocked accessors used by the webhook.
func TestSelectorAccessors(t *testing.T) {
	Reset([]byte(`
enabled: false
podSelector:
  app: demo
excludeNamespaces:
- ns-a
includeNamespaces:
- ns-b
`))
	if Enabled() {
		t.Error("expected disabled")
	}
	if PodSelector()["app"] != "demo" {
		t.Errorf("pod selector = %v", PodSelector())
	}
	if len(GetExcludeNamespaces()) != 1 || GetExcludeNamespaces()[0] != "ns-a" {
		t.Errorf("exclude namespaces = %v", GetExcludeNamespaces())
	}
	if len(GetIncludeNamespaces()) != 1 || GetIncludeNamespaces()[0] != "ns-b" {
		t.Errorf("include namespaces = %v", GetIncludeNamespaces())
	}
}
