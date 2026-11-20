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
	"context"
	"log"
	"os"

	"github.com/ketches/registry-proxy/internal/config"
	"github.com/ketches/registry-proxy/internal/global"
	"github.com/ketches/registry-proxy/internal/rules"
)

// startFileWatcher optionally enables file-system hot reload. It is only
// activated when the deployment sets REGISTRY_PROXY_CONFIG_FILE to the path
// of the mounted configuration (for example a ConfigMap volume mount). When
// unset, the ConfigMap informer remains the sole reload source and behavior
// is unchanged for existing deployments.
//
// The watcher observes the directory containing the file, which makes
// Kubernetes' atomic symlink-swap ConfigMap updates visible; see
// rules.NewFSNotifyDetector for the rationale.
func startFileWatcher(ctx context.Context) {
	path := os.Getenv(global.ConfigFileEnv)
	if path == "" {
		return
	}

	// Initial load so the engine has a snapshot before serving, even if the
	// informer has not delivered yet. Failure here is non-fatal: the engine
	// keeps any previously loaded rules (or a nil snapshot, under which
	// rewriting is a pass-through) and retries on every later change event.
	if _, err := config.Engine().ReloadFromFile(path); err != nil {
		log.Printf("Initial rule file load from %s failed: %v", path, err)
	}

	detector, err := rules.NewFSNotifyDetector(path)
	if err != nil {
		log.Printf("Start file watcher for %s failed, falling back to informer-only reloads: %v", path, err)
		return
	}
	config.Engine().AttachFile(path)
	config.Engine().Start(ctx, detector)
	log.Printf("Rule file hot reload enabled, watching %s (parent directory based, symlink-swap safe)", path)
}
