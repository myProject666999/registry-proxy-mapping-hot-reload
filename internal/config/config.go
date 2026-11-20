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
	"log"
	"sync"

	"github.com/ketches/registry-proxy/internal/rules"
	"github.com/ketches/registry-proxy/pkg/util"
	"k8s.io/apimachinery/pkg/labels"
)

// config is the registry proxy config
type config struct {
	// Enabled is the flag to enable the registry proxy
	Enabled bool `yaml:"enabled"`
	// Proxies is the map of registry domain and proxy domain (legacy format,
	// still accepted by the rule engine)
	Proxies map[string]string `yaml:"proxies"`
	// Rules is the explicit prefix rule list; it is parsed and validated by
	// the rule engine (see internal/rules), not by this struct directly.
	// ExcludeNamespaces is the list of namespaces that will not be proxied
	ExcludeNamespaces []string `yaml:"excludeNamespaces"`
	// IncludeNamespaces is the list of namespaces that will be proxied
	IncludeNamespaces []string `yaml:"includeNamespaces"`
	// PodSelector is the selector to select the pods that will be proxied
	PodSelector labels.Set `yaml:"podSelector"`
	// NamespaceSelector is the selector to select the namespaces that will be proxied
	NamespaceSelector labels.Set `yaml:"namespaceSelector"`
}

var defaultProxies = map[string]string{
	"docker.io":       "docker.linkos.org",
	"registry.k8s.io": "k8s.linkos.org",
	"quay.io":         "quay.linkos.org",
	"ghcr.io":         "ghcr.linkos.org",
	"gcr.io":          "gcr.linkos.org",
}

var defaultConfig = config{
	Enabled: true,
	Proxies: defaultProxies,
	ExcludeNamespaces: []string{
		"kube-system",
		"kube-public",
		"kube-node-lease",
		"registry-proxy",
	},
	IncludeNamespaces: []string{
		"*",
	},
	PodSelector:       labels.Set{},
	NamespaceSelector: labels.Set{},
}

// configInstance is the singleton config instance. Only the non-rule fields
// (selectors, namespaces, enabled) are read from it; image rewriting goes
// through the rule engine's atomic snapshot instead.
var (
	configInstance = &defaultConfig
	configMu       sync.RWMutex
)

// engine is the process-wide rule engine. Its active snapshot is swapped
// atomically on every successful (re)load, so admission requests never take a
// lock while matching rules. Prometheus metrics expose the active version,
// checksum and reload success/failure counters.
var engine = rules.NewEngine(
	"configmap://registry-proxy-config/config.yaml",
	rules.WithMetrics(rules.NewPrometheusMetrics(nil)),
)

// Engine returns the process-wide rule engine.
func Engine() *rules.Engine {
	return engine
}

// Get get the singleton config instance
func Get() *config {
	configMu.RLock()
	defer configMu.RUnlock()
	return configInstance
}

// GetExcludeNamespaces get the singleton config instance's excludeNamespaces
func GetExcludeNamespaces() []string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configInstance.ExcludeNamespaces
}

// GetIncludeNamespaces get the singleton config instance's includeNamespaces
func GetIncludeNamespaces() []string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configInstance.IncludeNamespaces
}

// Enabled get the singleton config instance's enabled
func Enabled() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return configInstance.Enabled
}

// PodSelector get the singleton config instance's pod selector
func PodSelector() labels.Set {
	configMu.RLock()
	defer configMu.RUnlock()
	return configInstance.PodSelector
}

// NamespaceSelector get the singleton config instance's namespace selector
func NamespaceSelector() labels.Set {
	configMu.RLock()
	defer configMu.RUnlock()
	return configInstance.NamespaceSelector
}

// Reset replaces the singleton config from raw YAML. If in is empty, the
// built-in default configuration is restored.
//
// The non-rule fields are swapped in directly, while the rule engine runs its
// full parse/validate/build pipeline: when rule loading fails the previous
// rules stay active, the error is logged with the offending rule's location,
// and the failure is exposed through engine metrics. The non-rule fields are
// still applied so a bad rule edit cannot block unrelated settings updates.
func Reset(in []byte) {
	if len(in) == 0 {
		configMu.Lock()
		configInstance = &defaultConfig
		configMu.Unlock()
		out, _ := util.MarshalYAML(defaultConfig)
		if _, err := engine.LoadBytes(out, "default://config.yaml"); err != nil {
			log.Printf("Load default rules failed: %v", err)
		}
		printCurrentConfig()
		return
	}

	next := &config{}
	if err := util.UnmarshalYAML(in, next); err != nil {
		log.Printf("Reset config failed, keeping previous config and rules: %v", err)
		return
	}

	configMu.Lock()
	configInstance = next
	configMu.Unlock()

	// Rule load failures are contained inside LoadBytes: the old snapshot
	// remains active and the error is logged with rule location and reason.
	if _, err := engine.LoadBytes(in, "configmap://registry-proxy-config/config.yaml"); err != nil {
		log.Printf("Load rules from config failed, previous rules remain active: %v", err)
	}
	printCurrentConfig()
}

// printCurrentConfig print current config
func printCurrentConfig() {
	out, err := util.MarshalYAML(configInstance)
	if err != nil {
		log.Printf("Marshal config failed: %v", err)
		return
	}
	log.Printf("Current registry proxy config: \n%s", string(out))
}
