package config

import "testing"

func TestLoadDefaultsAndRejectsShortLease(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "IRS_BASE_URL", "IRS_BEARER_TOKEN", "HTTP_ADDR", "WORKER_IDLE_DELAY", "WORKER_LEASE_DURATION", "HTTP_CONNECT_TIMEOUT", "HTTP_RESPONSE_HEADER_TIMEOUT", "HTTP_TOTAL_TIMEOUT", "SHUTDOWN_TIMEOUT", "WORKER_MAX_ATTEMPTS"} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_URL", defaultDatabaseURL)
	t.Setenv("HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("WORKER_IDLE_DELAY", "1s")
	t.Setenv("WORKER_LEASE_DURATION", "90s")
	t.Setenv("HTTP_CONNECT_TIMEOUT", "3s")
	t.Setenv("HTTP_RESPONSE_HEADER_TIMEOUT", "5s")
	t.Setenv("HTTP_TOTAL_TIMEOUT", "10s")
	t.Setenv("SHUTDOWN_TIMEOUT", "15s")
	config, err := Load()
	if err != nil || config.MaxAttempts != 1000 {
		t.Fatalf("load defaults: %+v, %v", config, err)
	}
	t.Setenv("WORKER_LEASE_DURATION", "70s")
	if _, err := Load(); err == nil {
		t.Fatal("short lease accepted")
	}
}
