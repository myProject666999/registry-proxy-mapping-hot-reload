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

	"gopkg.in/yaml.v3"
)

// ruleEntry is the on-disk shape of a rule.
type ruleEntry struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// Parse reads YAML configuration and returns the rules in configuration
// order, each annotated with the Location it was declared at. Two shapes are
// accepted:
//
//	rules:
//	- source: docker.io
//	  target: proxy-a.example
//
// and the legacy flat map (still used by existing registry-proxy configs):
//
//	proxies:
//	  docker.io: docker.linkos.org
//
// Line numbers are captured from the YAML nodes themselves so that later
// validation errors can point at exact lines. Parse only performs structural
// and per-rule validation; cross-rule validation (duplicate prefixes, cycles)
// happens in Build, which owns the matching semantics.
func Parse(data []byte, file string) ([]Rule, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// yaml errors carry their own line/column in the message already.
		return nil, ruleError(Location{File: file}, "parse YAML: %v", err)
	}

	// An empty document (or one containing only null) decodes to a node with
	// no content. The caller decides whether an empty file is allowed; Parse
	// itself treats "no rules found" as an empty result without erroring.
	top := documentMapping(&root)
	if top == nil {
		return nil, nil
	}

	var rules []Rule
	for i := 0; i+1 < len(top.Content); i += 2 {
		keyNode := top.Content[i]
		valNode := top.Content[i+1]
		switch keyNode.Value {
		case "rules":
			parsed, err := parseRulesList(valNode, file)
			if err != nil {
				return nil, err
			}
			rules = append(rules, parsed...)
		case "proxies":
			parsed, err := parseLegacyProxies(valNode, file)
			if err != nil {
				return nil, err
			}
			rules = append(rules, parsed...)
		default:
			// Other top-level keys (enabled, excludeNamespaces, ...) belong to
			// the rest of the application config and are intentionally ignored
			// by the rule engine.
		}
	}
	return rules, nil
}

// documentMapping returns the mapping node of the YAML document, or nil when
// the document is empty/null.
func documentMapping(root *yaml.Node) *yaml.Node {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	return doc
}

// parseRulesList parses the explicit "rules" sequence.
func parseRulesList(seq *yaml.Node, file string) ([]Rule, error) {
	if seq.Kind != yaml.SequenceNode {
		return nil, ruleError(Location{File: file, Line: seq.Line}, `"rules" must be a sequence`)
	}
	var out []Rule
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			return nil, ruleError(Location{File: file, Line: item.Line}, "each rule must be a mapping with source and target")
		}
		var entry ruleEntry
		if err := item.Decode(&entry); err != nil {
			return nil, ruleError(Location{File: file, Line: item.Line}, "decode rule: %v", err)
		}
		// Anchor the location at the "source" key line when present, which is
		// where a reader looks first; fall back to the node start line.
		line := item.Line
		for i := 0; i+1 < len(item.Content); i += 2 {
			if item.Content[i].Value == "source" {
				line = item.Content[i].Line
				break
			}
		}
		rule, err := newRule(entry.Source, entry.Target, Location{File: file, Line: line})
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}

// parseLegacyProxies parses the legacy "proxies" mapping, locating each entry
// at the line of its key.
func parseLegacyProxies(mapping *yaml.Node, file string) ([]Rule, error) {
	if mapping.Kind != yaml.MappingNode {
		return nil, ruleError(Location{File: file, Line: mapping.Line}, `"proxies" must be a mapping`)
	}
	var out []Rule
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		keyNode := mapping.Content[i]
		valNode := mapping.Content[i+1]
		var target string
		if err := valNode.Decode(&target); err != nil {
			return nil, ruleError(Location{File: file, Line: keyNode.Line}, "decode proxy target: %v", err)
		}
		rule, err := newRule(keyNode.Value, target, Location{File: file, Line: keyNode.Line})
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}

// newRule validates a single raw rule and builds its normalized form.
func newRule(source, target string, loc Location) (Rule, error) {
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	if source == "" {
		return Rule{}, ruleError(loc, "rule source must not be empty")
	}
	if target == "" {
		return Rule{}, ruleError(loc, "rule target for %q must not be empty", source)
	}
	if strings.Contains(source, "://") {
		return Rule{}, ruleError(loc, "rule source %q must not include a scheme; use host[/path] without %s", source, source[:strings.Index(source, "://")+3])
	}
	sourceSegs := normalizePrefix(source)
	if len(sourceSegs) == 0 {
		return Rule{}, ruleError(loc, "rule source %q has no host segment", source)
	}
	targetSegs := normalizePrefix(target)
	if len(targetSegs) == 0 {
		return Rule{}, ruleError(loc, "rule target %q has no host segment", target)
	}
	return Rule{
		Source:     source,
		Target:     target,
		SourceSegs: sourceSegs,
		TargetSegs: targetSegs,
		TargetRaw:  target,
		Location:   loc,
	}, nil
}

// describeRules renders a short, deterministic content summary of a rule set
// for load-success logs.
func describeRules(rules []Rule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, r.summary())
	}
	return strings.Join(parts, "; ")
}
