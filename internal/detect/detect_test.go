package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectTermuxLayout(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "opencode2api", "config.json"), `{
  // local gateway
  "listen": "127.0.0.1:8090",
  "server_keys": ["sk-local-test"],
}`)
	writeFile(t, filepath.Join(home, ".local", "share", "opencode", "auth.json"), `{
  "opencode": {"type": "api", "key": "sk-zen-test"}
}`)

	res := Detect(Options{Home: home})
	if res.Opencode2APIKey != "sk-local-test" {
		t.Fatalf("gateway key = %q", res.Opencode2APIKey)
	}
	if res.Opencode2APIURL != "http://127.0.0.1:8090/v1" {
		t.Fatalf("gateway URL = %q", res.Opencode2APIURL)
	}
	if res.ZenKey != "sk-zen-test" {
		t.Fatalf("zen key = %q", res.ZenKey)
	}
	if res.ZenAuthFile == "" || res.Opencode2APIConfig == "" {
		t.Fatalf("paths not reported: %+v", res)
	}
}

func TestDetectWildcardListen(t *testing.T) {
	for listen, want := range map[string]string{
		"0.0.0.0:8080":   "http://127.0.0.1:8080/v1",
		":8080":          "http://127.0.0.1:8080/v1",
		"[::]:8080":      "http://127.0.0.1:8080/v1",
		"127.0.0.1:1234": "http://127.0.0.1:1234/v1",
		"localhost:8080": "http://localhost:8080/v1",
		"https://x:8443": "https://x:8443/v1",
		"":               "http://127.0.0.1:8080/v1",
	} {
		if got := gatewayURL(listen); got != want {
			t.Errorf("gatewayURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestDetectExplicitPaths(t *testing.T) {
	dir := t.TempDir()
	gateway := filepath.Join(dir, "gw.json")
	auth := filepath.Join(dir, "auth.json")
	writeFile(t, gateway, `{"listen":"127.0.0.1:7070","server_keys":["k1","k2"]}`)
	writeFile(t, auth, `{"opencode":{"key":"z1"}}`)

	res := Detect(Options{Home: filepath.Join(dir, "none"), Opencode2APIConfig: gateway, ZenAuthFile: auth})
	if res.Opencode2APIKey != "k1" || res.Opencode2APIURL != "http://127.0.0.1:7070/v1" {
		t.Fatalf("gateway = %+v", res)
	}
	if res.ZenKey != "z1" {
		t.Fatalf("zen = %q", res.ZenKey)
	}
}

func TestDetectMissing(t *testing.T) {
	if _, err := os.Stat("/data/data/com.termux/files/home/opencode2api/config.json"); err == nil {
		t.Skip("live Termux fallback exists in this environment")
	}
	res := Detect(Options{Home: t.TempDir()})
	if res.Opencode2APIKey != "" || res.ZenKey != "" {
		t.Fatalf("unexpected detection")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
