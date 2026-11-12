package ghapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DiskCache keeps JSON blobs for data that never changes once a run has
// completed (a run attempt's jobs, the cache events parsed from a job log).
// A nil *DiskCache is valid and caches nothing.
type DiskCache struct {
	Dir string
}

// DefaultCacheDir is <user cache dir>/gha-cache-optimizer/<owner>/<repo>.
func DefaultCacheDir(owner, repo string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gha-cache-optimizer", owner, repo), nil
}

func (d *DiskCache) path(key string) string {
	// Keys are generated internally, but keep them to one path segment anyway.
	key = strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(key)
	return filepath.Join(d.Dir, key+".json")
}

// Load decodes the blob for key into v and reports whether it was found.
func (d *DiskCache) Load(key string, v any) bool {
	if d == nil || d.Dir == "" {
		return false
	}
	data, err := os.ReadFile(d.path(key))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// Store writes v under key. Failures are ignored: the cache is an optimization.
func (d *DiskCache) Store(key string, v any) {
	if d == nil || d.Dir == "" {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return
	}
	// Write then rename so a concurrent or interrupted run never reads half a file.
	tmp, err := os.CreateTemp(d.Dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), d.path(key)); err != nil {
		os.Remove(tmp.Name())
	}
}
