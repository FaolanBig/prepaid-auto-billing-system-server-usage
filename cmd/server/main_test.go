package main

import (
	"testing"

	"github.com/FaolanBig/prepaid-auto-billing-system-server-usage/internal/config"
)

func TestPostgresPoolConfigUsesConfiguredHostForTLSFallback(t *testing.T) {
	poolConfig, err := postgresPoolConfig(config.Config{
		DatabaseHost: "postgres",
		DatabasePort: "5432",
		DatabaseName: "powerbilling",
		DatabaseUser: "powerbilling",
		DatabasePass: "test-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := poolConfig.ConnConfig.Host; got != "postgres" {
		t.Fatalf("primary host = %q, want postgres", got)
	}
	if got := poolConfig.ConnConfig.Port; got != 5432 {
		t.Fatalf("primary port = %d, want 5432", got)
	}
	if len(poolConfig.ConnConfig.Fallbacks) == 0 {
		t.Fatal("expected pgx TLS fallback configuration")
	}
	for _, fallback := range poolConfig.ConnConfig.Fallbacks {
		if fallback.Host != "postgres" || fallback.Port != 5432 {
			t.Errorf("fallback endpoint = %s:%d, want postgres:5432", fallback.Host, fallback.Port)
		}
	}
}

func TestPostgresPoolConfigRejectsInvalidPort(t *testing.T) {
	_, err := postgresPoolConfig(config.Config{
		DatabaseHost: "postgres",
		DatabasePort: "not-a-port",
	})
	if err == nil {
		t.Fatal("expected invalid PostgreSQL port to fail")
	}
}
