package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	os.Clearenv()
	cfg := Load()
	if cfg.Port != ":8080" {
		t.Errorf("Port = %q, want :8080", cfg.Port)
	}
	if cfg.DBPath != "meshdns.db" {
		t.Errorf("DBPath = %q, want meshdns.db", cfg.DBPath)
	}
	if cfg.ProbeInterval != 60*time.Second {
		t.Errorf("ProbeInterval = %v, want 60s", cfg.ProbeInterval)
	}
	if cfg.ProbeTimeout != 5*time.Second {
		t.Errorf("ProbeTimeout = %v, want 5s", cfg.ProbeTimeout)
	}
	if cfg.Workers != 8 {
		t.Errorf("Workers = %d, want 8", cfg.Workers)
	}
}

func TestLoadFromEnv(t *testing.T) {
	os.Setenv("MESHDNS_PORT", ":9090")
	os.Setenv("MESHDNS_DB", "/tmp/test.db")
	os.Setenv("MESHDNS_PROBE_INTERVAL", "30s")
	os.Setenv("MESHDNS_PROBE_TIMEOUT", "3s")
	os.Setenv("MESHDNS_WORKERS", "4")
	defer os.Clearenv()

	cfg := Load()
	if cfg.Port != ":9090" {
		t.Errorf("Port = %q, want :9090", cfg.Port)
	}
	if cfg.DBPath != "/tmp/test.db" {
		t.Errorf("DBPath = %q, want /tmp/test.db", cfg.DBPath)
	}
	if cfg.ProbeInterval != 30*time.Second {
		t.Errorf("ProbeInterval = %v, want 30s", cfg.ProbeInterval)
	}
	if cfg.ProbeTimeout != 3*time.Second {
		t.Errorf("ProbeTimeout = %v, want 3s", cfg.ProbeTimeout)
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers = %d, want 4", cfg.Workers)
	}
}

func TestLoadInvalidDuration(t *testing.T) {
	os.Setenv("MESHDNS_PROBE_INTERVAL", "bad")
	defer os.Clearenv()
	cfg := Load()
	if cfg.ProbeInterval != 60*time.Second {
		t.Errorf("should fall back to default on invalid duration, got %v", cfg.ProbeInterval)
	}
}