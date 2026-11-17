package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("UP_KEY", "secret-upstream")
	t.Setenv("CLIENT_KEY", "secret-client")
	t.Setenv("HDR", "header-value")

	path := writeConfigFile(t, `{
		"listen": "127.0.0.1:19090",
		"admin_listen": "",
		"max_attempts": 3,
		"max_request_bytes": 1048576,
		"upstream_header_timeout_seconds": 30,
		"max_cooldown_seconds": 300,
		"client_api_key": "${CLIENT_KEY}",
		"log_level": "debug",
		"retry_statuses": [429, 502],
		"cooldown_seconds": {"429": 11, "network": 4},
		"upstreams": [
			{"name": "local", "base_url": "http://127.0.0.1:8080/v1/", "priority": 0},
			{"name": "cloud", "base_url": "https://example.com/v1", "api_key": "${UP_KEY}",
			 "auth_header": "api-key", "auth_prefix": "", "priority": 5,
			 "headers": {"X-Test": "${HDR}"}},
			{"name": "disabled", "base_url": "https://disabled.example/v1", "enabled": false}
		]
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:19090" || cfg.AdminListen != "" {
		t.Fatalf("listen = %q admin = %q", cfg.Listen, cfg.AdminListen)
	}
	if cfg.MaxAttempts != 3 || cfg.MaxRequestBytes != 1048576 {
		t.Fatalf("attempts/bytes = %d/%d", cfg.MaxAttempts, cfg.MaxRequestBytes)
	}
	if cfg.HeaderTimeout != 30*time.Second || cfg.MaxCooldown != 300*time.Second {
		t.Fatalf("timeouts = %s/%s", cfg.HeaderTimeout, cfg.MaxCooldown)
	}
	if cfg.LogLevel != LevelDebug || cfg.ClientAPIKey != "secret-client" {
		t.Fatalf("log level/client key = %v/%q", cfg.LogLevel, cfg.ClientAPIKey)
	}
	if !cfg.RetryStatuses[429] || !cfg.RetryStatuses[502] || cfg.RetryStatuses[403] {
		t.Fatalf("retry statuses = %+v", cfg.RetryStatuses)
	}
	if cfg.Cooldowns[429] != 11*time.Second || cfg.Cooldowns[403] != 120*time.Second {
		t.Fatalf("cooldowns = %+v (expected defaults to merge)", cfg.Cooldowns)
	}
	if cfg.NetworkCooldown != 4*time.Second {
		t.Fatalf("network cooldown = %s", cfg.NetworkCooldown)
	}
	if len(cfg.Upstreams) != 2 {
		t.Fatalf("upstreams = %d, want 2 (disabled skipped)", len(cfg.Upstreams))
	}

	local := cfg.Upstreams[0]
	if local.Name != "local" || local.Base.Path != "/v1" || local.Priority != 0 || local.APIKey != "" {
		t.Fatalf("local = %+v", local)
	}
	if got := local.TargetURL("/v1/models", ""); got != "http://127.0.0.1:8080/v1/models" {
		t.Fatalf("local target = %s", got)
	}

	cloud := cfg.Upstreams[1]
	if cloud.APIKey != "secret-upstream" || cloud.AuthHeader != "api-key" || cloud.AuthPrefix != "" {
		t.Fatalf("cloud auth = %+v", cloud)
	}
	if got := cloud.Headers.Get("X-Test"); got != "header-value" {
		t.Fatalf("expanded header = %q", got)
	}
}

func TestConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name, contents, want string
	}{
		{
			name:     "unknown field",
			contents: `{"upstreams":[{"base_url":"http://127.0.0.1:1/v1"}],"typo":true}`,
			want:     "unknown field",
		},
		{
			name:     "no upstreams",
			contents: `{"upstreams":[]}`,
			want:     "no enabled upstreams",
		},
		{
			name:     "bad scheme",
			contents: `{"upstreams":[{"base_url":"ftp://example.com/v1"}]}`,
			want:     "http or https",
		},
		{
			name:     "bad retry status",
			contents: `{"retry_statuses":[200],"upstreams":[{"base_url":"http://127.0.0.1:1/v1"}]}`,
			want:     "not an HTTP error status",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tc.contents))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestConfigDeduplicatesNames(t *testing.T) {
	path := writeConfigFile(t, `{"upstreams":[
		{"name":"dup","base_url":"http://127.0.0.1:1/v1"},
		{"name":"dup","base_url":"http://127.0.0.1:2/v1"}
	]}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstreams[0].Name != "dup" || cfg.Upstreams[1].Name != "dup-2" {
		t.Fatalf("names = %q, %q", cfg.Upstreams[0].Name, cfg.Upstreams[1].Name)
	}
}
