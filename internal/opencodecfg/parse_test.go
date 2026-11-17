package opencodecfg

import (
	"strings"
	"testing"
)

func TestParseJSONC(t *testing.T) {
	src := []byte(`{
  // top comment
  "$schema": "https://opencode.ai/config.json",
  /* block
     comment */
  "plugins": ["./plugins/a",],
  "commands": {
    "x": {"template": "line\nwith \"quotes\" and \u263a"},
  },
}`)
	doc, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, _ := FindString(doc, "plugins"); got != "" {
		t.Fatalf("FindString on array returned %q", got)
	}
	if got, ok := FindString(doc, "$schema"); !ok || got != "https://opencode.ai/config.json" {
		t.Fatalf("$schema = %q, %v", got, ok)
	}
	if got, ok := FindString(doc, "commands", "x", "template"); !ok || got != "line\nwith \"quotes\" and ☺" {
		t.Fatalf("template = %q, %v", got, ok)
	}
}

func TestParseDuplicateKey(t *testing.T) {
	_, err := Parse([]byte(`{"a": 1, "a": 2}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate key error, got %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []string{
		``,
		`[]`,
		`{"a": }`,
		`{"a": 1`,
		`{"a": 1} trailing`,
		`{"a": /* unterminated`,
		`{"a": "unterminated}`,
	}
	for _, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", src)
		}
	}
}

func TestParseBOM(t *testing.T) {
	src := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"a": true}`)...)
	if _, err := Parse(src); err != nil {
		t.Fatalf("Parse with BOM: %v", err)
	}
}
