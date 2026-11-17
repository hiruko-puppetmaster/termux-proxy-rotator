package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxyrotator/internal/opencodecfg"
)

const catalogFixture = `{
  "object": "list",
  "data": [
    {
      "id": "other-free",
      "context_window": 32000,
      "max_output": 4096,
      "tool_call": true,
      "reasoning": false,
      "metadata": {
        "input_modalities": ["text"],
        "output_modalities": ["text"]
      }
    },
    {
      "id": "deepseek-v4.1-flash-free",
      "name": "DeepSeek V4.1 Flash (free)",
      "context_window": 200000,
      "max_output": 32000,
      "tool_call": true,
      "reasoning": true,
      "reasoning_efforts": ["low", "medium", "high"],
      "metadata": {
        "context_window": 200000,
        "max_output": 32000,
        "tool_call": true,
        "reasoning": true,
        "input_modalities": ["text", "image"],
        "output_modalities": ["text"]
      }
    }
  ]
}`

func TestApplyCommand(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "opencode.jsonc")
	modelsPath := filepath.Join(dir, "models.json")
	original := `{
  "$schema": "https://opencode.ai/config.json",
  // keep this comment
  "plugins": ["./plugins/unlimited-shield"],
  "commands": {
    "hello": {"template": "say hello"}
  }
}
`
	writeTestFile(t, configPath, original)
	writeTestFile(t, modelsPath, catalogFixture)

	args := []string{
		"apply",
		"-config", configPath,
		"-models-file", modelsPath,
		"-api-key", "{file:/tmp/rotator/client.key}",
		"-base-url", "http://127.0.0.1:9090/v1",
		"-no-backup",
		"-quiet",
	}
	if code := run(args); code != 0 {
		t.Fatalf("run returned %d", code)
	}
	out, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		"// keep this comment",
		`"plugins": ["./plugins/unlimited-shield"]`,
		`"model": "rotator/deepseek-v4.1-flash-free"`,
		`"other-free"`,
		`"deepseek-v4.1-flash-free"`,
		`"baseURL": "http://127.0.0.1:9090/v1"`,
		`"apiKey": "{file:/tmp/rotator/client.key}"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("config missing %q:\n%s", want, text)
		}
	}
	doc, err := opencodecfg.Parse(out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if got, _ := opencodecfg.FindString(doc, "model"); got != "rotator/deepseek-v4.1-flash-free" {
		t.Fatalf("default = %q", got)
	}

	// Re-running must not change the file.
	first := string(out)
	if code := run(args); code != 0 {
		t.Fatalf("second run returned %d", code)
	}
	second, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if first != string(second) {
		t.Fatalf("second run changed the file\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestApplyCommandKeepsExistingRotatorModel(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "opencode.jsonc")
	modelsPath := filepath.Join(dir, "models.json")
	writeTestFile(t, configPath, `{"model": "rotator/other-free"}`)
	writeTestFile(t, modelsPath, catalogFixture)
	args := []string{
		"apply",
		"-config", configPath,
		"-models-file", modelsPath,
		"-api-key", "{file:/tmp/key}",
		"-no-backup",
		"-quiet",
	}
	if code := run(args); code != 0 {
		t.Fatalf("run returned %d", code)
	}
	out, _ := os.ReadFile(configPath)
	doc, err := opencodecfg.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := opencodecfg.FindString(doc, "model"); got != "rotator/other-free" {
		t.Fatalf("default = %q, want existing rotator model", got)
	}
}

func TestApplyCommandRejectsMissingExplicitDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "opencode.jsonc")
	modelsPath := filepath.Join(dir, "models.json")
	writeTestFile(t, configPath, "{}")
	writeTestFile(t, modelsPath, catalogFixture)
	code := run([]string{
		"apply",
		"-config", configPath,
		"-models-file", modelsPath,
		"-api-key", "x",
		"-default-model", "does-not-exist",
		"-no-backup",
		"-quiet",
	})
	if code == 0 {
		t.Fatal("run succeeded with a missing explicit default")
	}
	out, _ := os.ReadFile(configPath)
	if string(out) != "{}" {
		t.Fatalf("config was modified on failure:\n%s", out)
	}
}

func TestBuildProviderMetadata(t *testing.T) {
	models, err := loadCatalog(applyOptions{modelsFile: writeTempFile(t, catalogFixture)})
	if err != nil {
		t.Fatal(err)
	}
	provider := buildProvider(models, applyOptions{
		baseURL: "http://127.0.0.1:9090/v1",
		apiKey:  "{file:/tmp/key}",
		name:    "Test",
	})
	modelMap := provider["models"].(map[string]any)
	ds := modelMap["deepseek-v4.1-flash-free"].(map[string]any)
	caps := ds["capabilities"].(map[string]any)
	if caps["tools"] != true {
		t.Errorf("tools = %v", caps["tools"])
	}
	input := caps["input"].([]string)
	if len(input) != 2 || input[1] != "image" {
		t.Errorf("input = %v", input)
	}
	reasoning := caps["reasoning"].(map[string]any)
	if reasoning["supported"] != true {
		t.Errorf("reasoning = %v", reasoning)
	}
	effort := reasoning["effort"].([]string)
	if len(effort) != 3 {
		t.Errorf("effort = %v", effort)
	}
	limit := ds["limit"].(map[string]any)
	if limit["context"] != int64(200000) || limit["output"] != int64(32000) {
		t.Errorf("limit = %v", limit)
	}

	other := modelMap["other-free"].(map[string]any)
	otherCaps := other["capabilities"].(map[string]any)
	otherReasoning := otherCaps["reasoning"].(map[string]any)
	if otherReasoning["supported"] != false {
		t.Errorf("other reasoning = %v", otherReasoning)
	}
}

func TestChooseDefault(t *testing.T) {
	models, err := loadCatalog(applyOptions{modelsFile: writeTempFile(t, catalogFixture)})
	if err != nil {
		t.Fatal(err)
	}
	base := applyOptions{providerID: "rotator", preferModel: "deepseek-v4.1-flash-free"}
	if got, err := chooseDefault(models, "", base); err != nil || got != "deepseek-v4.1-flash-free" {
		t.Fatalf("preferred = %q, %v", got, err)
	}
	if got, err := chooseDefault(models, "rotator/other-free", base); err != nil || got != "other-free" {
		t.Fatalf("existing = %q, %v", got, err)
	}
	opts := base
	opts.keepDefault = true
	if got, err := chooseDefault(models, "other/provider", opts); err != nil || got != "" {
		t.Fatalf("keep default = %q, %v", got, err)
	}
	opts = base
	opts.preferModel = "missing"
	if got, err := chooseDefault(models, "other/provider", opts); err != nil || got != "deepseek-v4.1-flash-free" {
		t.Fatalf("fallback = %q, %v", got, err)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"abc":         "'abc'",
		"a'b":         `'a'\''b'`,
		"line\nbreak": "'line\nbreak'",
		"":            "''",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	writeTestFile(t, path, content)
	return path
}
