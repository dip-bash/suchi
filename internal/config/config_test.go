package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultsFollowXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	t.Setenv("XDG_STATE_HOME", "/state")
	t.Setenv("SUCHI_DB", "")
	t.Setenv("SUCHI_LIMIT", "")
	c := Default()
	if c.DBPath != filepath.Join("/data", "suchi", "suchi.db") ||
		c.PIDPath() != filepath.Join("/state", "suchi", "suchi.pid") ||
		c.LogPath() != filepath.Join("/state", "suchi", "suchi.log") {
		t.Fatalf("got %+v", c)
	}
	if c.Limit != DefaultLimit || c.MaxBytes != DefaultMaxBytes || c.Selection != "clipboard" || c.Backend != "auto" {
		t.Fatalf("got %+v", c)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("SUCHI_DB", "/tmp/x.db")
	t.Setenv("SUCHI_LIMIT", "42")
	t.Setenv("SUCHI_MAX_BYTES", "not-a-number") // ignored, default kept
	c := Default()
	if c.DBPath != "/tmp/x.db" || c.Limit != 42 || c.MaxBytes != DefaultMaxBytes {
		t.Fatalf("got %+v", c)
	}
}

func TestValidate(t *testing.T) {
	good := Default()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"selection": func(c *Config) { c.Selection = "x" },
		"backend":   func(c *Config) { c.Backend = "x" },
		"limit":     func(c *Config) { c.Limit = 0 },
		"max-size":  func(c *Config) { c.MaxBytes = 0 },
	} {
		c := good
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
