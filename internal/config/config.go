// Package config resolves suchi's runtime settings: XDG paths, limits and
// backend selection. Environment variables provide defaults, command-line
// flags (applied by the cli package) override them.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Defaults for the tunable limits.
const (
	DefaultLimit    = 500
	DefaultMaxBytes = 1 << 20  // 1 MiB: larger clipboard text is not recorded
	DefaultMaxMedia = 16 << 20 // 16 MiB: larger images are not recorded
	// DefaultMediaBudget caps the total size of stored images; the oldest
	// unpinned ones are dropped first.
	DefaultMediaBudget = 256 << 20
)

// Config is the effective configuration of one suchi invocation.
type Config struct {
	DBPath    string // SQLite database
	StateDir  string // pid file and log
	Limit     int    // max clipboard entries kept (pinned entries are exempt)
	MaxBytes  int    // skip clipboard text larger than this
	MaxMedia  int    // skip images larger than this
	Selection string // "clipboard" or "primary"
	Backend   string // "auto", "wayland", "x11" or "tty"
}

// Default builds a Config from XDG directories and SUCHI_* environment variables.
func Default() Config {
	return Config{
		DBPath:    envStr("SUCHI_DB", filepath.Join(dataHome(), "suchi", "suchi.db")),
		StateDir:  filepath.Join(stateHome(), "suchi"),
		Limit:     envInt("SUCHI_LIMIT", DefaultLimit),
		MaxBytes:  envInt("SUCHI_MAX_BYTES", DefaultMaxBytes),
		MaxMedia:  envInt("SUCHI_MAX_MEDIA", DefaultMaxMedia),
		Selection: "clipboard",
		Backend:   envStr("SUCHI_BACKEND", "auto"),
	}
}

// PIDPath is the daemon's lock/pid file.
func (c Config) PIDPath() string { return filepath.Join(c.StateDir, "suchi.pid") }

// UIPIDPath is the lock of the open interface: only one suchi window runs at a time.
func (c Config) UIPIDPath() string { return filepath.Join(c.StateDir, "suchi-ui.pid") }

// LogPath is the daemon's log file.
func (c Config) LogPath() string { return filepath.Join(c.StateDir, "suchi.log") }

// Validate reports the first invalid setting.
func (c Config) Validate() error {
	switch c.Selection {
	case "clipboard", "primary":
	default:
		return fmt.Errorf("selection must be clipboard or primary, got %q", c.Selection)
	}
	switch c.Backend {
	case "auto", "wayland", "x11", "tty":
	default:
		return fmt.Errorf("backend must be auto, wayland, x11 or tty, got %q", c.Backend)
	}
	if c.Limit < 1 {
		return fmt.Errorf("limit must be at least 1")
	}
	if c.MaxBytes < 1 {
		return fmt.Errorf("max-size must be at least 1")
	}
	if c.MaxMedia < 0 {
		return fmt.Errorf("max-media must not be negative")
	}
	return nil
}

func dataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	return filepath.Join(home(), ".local", "share")
}

func stateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	return filepath.Join(home(), ".local", "state")
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
