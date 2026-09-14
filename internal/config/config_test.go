package config

import "testing"

func TestLoadRejectsInvalidLease(t *testing.T) {
	t.Setenv("NOTIFIER_MYSQL_DSN", "user:pass@tcp(localhost:3306)/db")
	t.Setenv("NOTIFIER_API_KEYS", "caller:key:user")
	t.Setenv("NOTIFIER_TARGETS_JSON", `[{"id":"t","callers":["caller"],"hosts":["example.com"],"ports":[443],"max_concurrency":1}]`)
	t.Setenv("NOTIFIER_REQUEST_TIMEOUT", "10s")
	t.Setenv("NOTIFIER_LEASE_DURATION", "5s")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid lease error")
	}
}

func TestLoadParsesPrincipalsAndTargets(t *testing.T) {
	t.Setenv("NOTIFIER_MYSQL_DSN", "user:pass@tcp(localhost:3306)/db")
	t.Setenv("NOTIFIER_API_KEYS", "caller:key:user,ops:secret:admin")
	t.Setenv("NOTIFIER_TARGETS_JSON", `[{"id":"crm","callers":["caller"],"hosts":["api.example.com"],"ports":[443],"max_concurrency":2}]`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKeys["secret"].Admin != true || cfg.Targets["crm"].MaxConcurrency != 2 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}
