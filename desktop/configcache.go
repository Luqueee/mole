package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Luqueee/mole/internal/config"
)

// Nearly every call from the frontend needs the config, several times a
// second while the window is open, so the last parse is kept until the file
// changes: a call then costs a stat instead of a read and a YAML parse.

// configStamp identifies one version of the config file.
type configStamp struct {
	path string // absolute: config.Find may answer relative to the working directory
	size int64
	mod  time.Time
}

func (s configStamp) same(o configStamp) bool {
	return s.path == o.path && s.size == o.size && s.mod.Equal(o.mod)
}

// racyWindow is the coarsest modification-time granularity expected (FAT
// keeps 2s). A file written this close to being read could be written again
// without its stamp changing, so such a parse is not kept.
const racyWindow = 2 * time.Second

var configCache struct {
	mu    sync.Mutex
	gen   uint64 // bumped by invalidateConfig: a parse begun before it is not kept
	stamp configStamp
	cfg   *config.Config
}

// loadConfig returns the active config and the path it came from (empty when
// there is none and the defaults apply). The *Config may be shared with other
// callers: treat it as read-only.
func loadConfig() (*config.Config, string, error) {
	path := config.Find()
	if path == "" {
		cfg, err := config.Load(path)
		return cfg, path, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	readAt := time.Now()
	fi, err := os.Stat(path)
	if err != nil { // gone since Find: Load decides between defaults and an error
		cfg, err := config.Load(path)
		return cfg, path, err
	}
	stamp := configStamp{path: abs, size: fi.Size(), mod: fi.ModTime()}

	configCache.mu.Lock()
	if configCache.cfg != nil && configCache.stamp.same(stamp) {
		cfg := configCache.cfg
		configCache.mu.Unlock()
		return cfg, path, nil
	}
	gen := configCache.gen
	configCache.mu.Unlock()

	cfg, err := config.Load(path)
	if err != nil {
		return nil, path, err
	}
	if stamp.mod.Before(readAt.Add(-racyWindow)) {
		configCache.mu.Lock()
		if configCache.gen == gen {
			configCache.stamp, configCache.cfg = stamp, cfg
		}
		configCache.mu.Unlock()
	}
	return cfg, path, nil
}

// invalidateConfig forgets the cached parse. Call it after anything the app
// runs that may write the config (the mole CLI is its only writer), so the
// next read does not depend on the file's stamp having changed.
func invalidateConfig() {
	configCache.mu.Lock()
	configCache.gen++
	configCache.cfg = nil
	configCache.mu.Unlock()
}
