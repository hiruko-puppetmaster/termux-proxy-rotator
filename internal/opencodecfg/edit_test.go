package opencodecfg

import (
	"encoding/json"
	"strings"
	"testing"
)

func providerFixture() map[string]any {
	return map[string]any{
		"name":    "Termux Proxy Rotator",
		"package": "@opencode/ai/providers/openai-compatible",
		"settings": map[string]any{
			"baseURL": "http://127.0.0.1:9090/v1",
			"apiKey":  "{file:/home/user/.config/opencode/rotator/client.key}",
		},
		"models": map[string]any{
			"test-model": map[string]any{
				"name": "Test Model",
				"limit": map[string]any{
					"context": 1000,
					"output":  100,
				},
			},
		},
	}
}

func TestApplyEmptyObject(t *testing.T) {
	out, err := Apply([]byte("{}"), Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc, err := Parse(out)
	if err != nil {
		t.Fatalf("output parse: %v\n%s", err, out)
	}
	if got, _ := FindString(doc, "model"); got != "rotator/test-model" {
		t.Fatalf("model = %q", got)
	}
	if doc.Root.Member("providers").Value.Member("rotator") == nil {
		t.Fatalf("rotator provider missing:\n%s", out)
	}
	if strings.Contains(string(out), "{}") {
		t.Fatalf("unexpected empty object:\n%s", out)
	}
}

func TestApplyPreservesCommentsAndUnrelatedKeys(t *testing.T) {
	src := `{
  "$schema": "https://opencode.ai/config.json",
  // keep this plugin exactly as it is
  "plugins": ["./plugins/unlimited-shield"],
  "commands": {
    // command comment
    "hello": {"template": "say hello"}
  }
}
`
	out, err := Apply([]byte(src), Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"// keep this plugin exactly as it is",
		`"plugins": ["./plugins/unlimited-shield"]`,
		"// command comment",
		`"hello": {"template": "say hello"}`,
		`"model": "rotator/test-model"`,
		`"providers": {`,
		`"rotator": {`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("output parse: %v\n%s", err, out)
	}
}

func TestApplyReplacesExistingBlockAndKeepsComments(t *testing.T) {
	src := `{
  "$schema": "https://opencode.ai/config.json",
  "model": "old/provider", // previous default
  "providers": {
    // a different provider stays untouched
    "other": {
      "name": "Other"
    },
    "rotator": {
      "name": "Old rotator"
    },
    "after": {
      "name": "After"
    }
  }
}
`
	out, err := Apply([]byte(src), Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, `"model": "rotator/test-model", // previous default`) {
		t.Errorf("model line not replaced cleanly:\n%s", out)
	}
	if !strings.Contains(text, "// a different provider stays untouched") {
		t.Errorf("comment lost:\n%s", out)
	}
	if !strings.Contains(text, `"other"`) || !strings.Contains(text, `"after"`) {
		t.Errorf("sibling providers lost:\n%s", out)
	}
	if strings.Contains(text, "Old rotator") {
		t.Errorf("old provider block not replaced:\n%s", out)
	}
	doc, err := Parse(out)
	if err != nil {
		t.Fatalf("output parse: %v\n%s", err, out)
	}
	rot := doc.Root.Member("providers").Value.Member("rotator")
	if got := rot.Value.Member("name").Value.Str; got != "Termux Proxy Rotator" {
		t.Fatalf("provider name = %q", got)
	}
}

func TestApplyAddsProviderToExistingProviders(t *testing.T) {
	src := `{
  "providers": {
    "other": {"name": "Other"} // trailing comment
  }
}`
	out, err := Apply([]byte(src), Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc, err := Parse(out)
	if err != nil {
		t.Fatalf("output parse: %v\n%s", err, out)
	}
	providers := doc.Root.Member("providers").Value
	if providers.Member("other") == nil || providers.Member("rotator") == nil {
		t.Fatalf("providers missing:\n%s", out)
	}
	if _, ok := FindString(doc, "model"); !ok {
		t.Fatalf("model missing:\n%s", out)
	}
}

func TestApplyAddsProviderToProvidersWithTrailingComma(t *testing.T) {
	src := `{
  "providers": {
    "other": {"name": "Other"},
  }
}`
	out, err := Apply([]byte(src), Update{
		ProviderID: "rotator",
		Provider:   providerFixture(),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("output parse: %v\n%s", err, out)
	}
	if strings.Count(string(out), `"rotator"`) != 1 {
		t.Fatalf("unexpected rotator count:\n%s", out)
	}
}

func TestApplyKeepsDefault(t *testing.T) {
	src := []byte(`{"model": "other/model"}`)
	out, err := Apply(src, Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
		KeepDefault:  true,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(string(out), `"model": "other/model"`) {
		t.Fatalf("default changed despite KeepDefault:\n%s", out)
	}
}

func TestApplyIdempotent(t *testing.T) {
	src := []byte(`{
  "$schema": "https://opencode.ai/config.json",
  // comment
  "plugins": []
}
`)
	u := Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
	}
	first, err := Apply(src, u)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := Apply(first, u)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("not idempotent\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestApplyPreservesCRLF(t *testing.T) {
	src := []byte("{\r\n  // comment\r\n  \"plugins\": []\r\n}\r\n")
	out, err := Apply(src, Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(strings.ReplaceAll(string(out), "\r\n", ""), "\n") {
		t.Fatalf("output contains bare LF:\n%q", out)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("output parse: %v\n%s", err, out)
	}
}

func TestApplyValidJSONValues(t *testing.T) {
	out, err := Apply([]byte(`{}`), Update{
		ProviderID:   "rotator",
		Provider:     providerFixture(),
		DefaultModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output is not strict JSON: %v\n%s", err, out)
	}
	if decoded["model"] != "rotator/test-model" {
		t.Fatalf("model = %v", decoded["model"])
	}
}
