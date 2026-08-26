package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration sourced from environment variables.
type Config struct {
	Port          string
	DBPath        string
	ProbeInterval time.Duration
	ProbeTimeout  time.Duration
	Workers       int
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Port:          envStr("MESHDNS_PORT", ":8080"),
		DBPath:        envStr("MESHDNS_DB", "meshdns.db"),
		ProbeInterval: envDuration("MESHDNS_PROBE_INTERVAL", 60*time.Second),
		ProbeTimeout:  envDuration("MESHDNS_PROBE_TIMEOUT", 5*time.Second),
		Workers:       envInt("MESHDNS_WORKERS", 8),
	}
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return fallback
}