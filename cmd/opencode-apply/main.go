// Command opencode-apply registers a Termux Proxy Rotator upstream in an
// OpenCode configuration and detects local upstream credentials.
//
// It edits OpenCode's JSONC configuration in place, preserving comments and
// unrelated settings, and validates the result before replacing the file.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"proxyrotator/internal/detect"
	"proxyrotator/internal/opencodecfg"
)

const version = "1.0.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		return runApply(nil)
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	case "-v", "--version", "version":
		fmt.Printf("opencode-apply %s\n", version)
		return 0
	case "apply":
		return runApply(args[1:])
	case "detect":
		return runDetect(args[1:])
	default:
		if strings.HasPrefix(args[0], "-") {
			return runApply(args)
		}
		fmt.Fprintf(os.Stderr, "opencode-apply: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `opencode-apply %s

Register a Termux Proxy Rotator provider in an OpenCode JSONC config.

Usage:
  opencode-apply apply [flags]    add or update the provider (default)
  opencode-apply detect [flags]   print detected upstream credentials
  opencode-apply --version

Run "opencode-apply apply -h" or "opencode-apply detect -h" for flags.
`, version)
}

// --- apply ---------------------------------------------------------------

type applyOptions struct {
	config       string
	providerID   string
	name         string
	baseURL      string
	apiKey       string
	modelsURL    string
	modelsFile   string
	keyFile      string
	defaultModel string
	preferModel  string
	keepDefault  bool
	dryRun       bool
	noBackup     bool
	backup       string
	timeout      time.Duration
	quiet        bool
}

func runApply(args []string) int {
	opts := applyOptions{
		providerID:  "rotator",
		name:        "Termux Proxy Rotator",
		baseURL:     "http://127.0.0.1:9090/v1",
		preferModel: "deepseek-v4.1-flash-free",
		timeout:     60 * time.Second,
	}
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.config, "config", "", "OpenCode config file (default: $OPENCODE_CONFIG or ~/.config/opencode/opencode.jsonc)")
	fs.StringVar(&opts.providerID, "provider-id", opts.providerID, "provider key to create or replace")
	fs.StringVar(&opts.name, "name", opts.name, "provider display name")
	fs.StringVar(&opts.baseURL, "base-url", opts.baseURL, "rotator base URL")
	fs.StringVar(&opts.apiKey, "api-key", "", "provider API key value (default: {file:<runtime>/client.key})")
	fs.StringVar(&opts.modelsURL, "models-url", "", "model catalog URL (default: <base-url>/models)")
	fs.StringVar(&opts.modelsFile, "models-file", "", "read the model catalog from a file instead of the network")
	fs.StringVar(&opts.keyFile, "key-file", "", "file containing the bearer key for the model catalog")
	fs.StringVar(&opts.defaultModel, "default-model", "", "explicit default model ID; fail if it is missing")
	fs.StringVar(&opts.preferModel, "prefer-model", opts.preferModel, "preferred default model ID, with fallback")
	fs.BoolVar(&opts.keepDefault, "keep-default", false, "do not change the top-level default model")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print the edited config instead of writing it")
	fs.BoolVar(&opts.noBackup, "no-backup", false, "do not create a backup file")
	fs.StringVar(&opts.backup, "backup", "", "backup path (default: <config>.bak)")
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "model catalog HTTP timeout")
	fs.BoolVar(&opts.quiet, "quiet", false, "only print errors")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "opencode-apply: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	home, _ := os.UserHomeDir()
	if home == "" {
		home = os.Getenv("HOME")
	}
	if opts.config == "" {
		opts.config = defaultConfigPath(home)
	}
	opts.config = expandHome(opts.config, home)
	if info, err := os.Stat(opts.config); err == nil && info.IsDir() {
		opts.config = filepath.Join(opts.config, "opencode.jsonc")
	}
	if opts.backup == "" {
		opts.backup = opts.config + ".bak"
	} else {
		opts.backup = expandHome(opts.backup, home)
	}
	runtimeDir := os.Getenv("ROTATOR_HOME")
	if runtimeDir == "" {
		runtimeDir = filepath.Join(configHome(home), "opencode", "rotator")
	}
	runtimeDir = expandHome(runtimeDir, home)
	if opts.apiKey == "" {
		opts.apiKey = "{file:" + filepath.Join(runtimeDir, "client.key") + "}"
	}
	if opts.keyFile == "" {
		opts.keyFile = filepath.Join(runtimeDir, "client.key")
	}
	if opts.modelsURL == "" {
		opts.modelsURL = strings.TrimSuffix(opts.baseURL, "/") + "/models"
	}

	original, existed, err := readOptional(opts.config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-apply: %v\n", err)
		return 1
	}

	models, err := loadCatalog(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-apply: %v\n", err)
		return 1
	}
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "opencode-apply: the model catalog is empty")
		return 1
	}

	existingDefault := ""
	if existed {
		if doc, err := opencodecfg.Parse(original); err == nil {
			existingDefault, _ = opencodecfg.FindString(doc, "model")
		}
	}
	defaultModel, err := chooseDefault(models, existingDefault, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-apply: %v\n", err)
		return 1
	}

	provider := buildProvider(models, opts)
	update := opencodecfg.Update{
		ProviderID:   opts.providerID,
		Provider:     provider,
		DefaultModel: defaultModel,
		KeepDefault:  opts.keepDefault,
	}
	out, err := opencodecfg.Apply(original, update)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-apply: %s: %v\n", opts.config, err)
		return 1
	}
	if err := verify(out, opts.providerID, len(models), defaultModel, opts.keepDefault); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-apply: refusing to write an invalid configuration: %v\n", err)
		return 1
	}

	if bytes.Equal(out, original) {
		if !opts.quiet {
			fmt.Printf("OpenCode config is already up to date: %s\n", opts.config)
		}
		return 0
	}
	if opts.dryRun {
		if !opts.quiet {
			fmt.Fprintf(os.Stderr, "dry run: no files were changed\n")
		}
		_, _ = os.Stdout.Write(out)
		return 0
	}
	if existed && !opts.noBackup {
		if _, err := os.Stat(opts.backup); errors.Is(err, os.ErrNotExist) {
			if err := copyFile(opts.config, opts.backup); err != nil {
				fmt.Fprintf(os.Stderr, "opencode-apply: backup: %v\n", err)
				return 1
			}
			if !opts.quiet {
				fmt.Printf("backup: %s\n", opts.backup)
			}
		}
	}
	if err := writeFileAtomic(opts.config, out); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-apply: write: %v\n", err)
		return 1
	}
	if !opts.quiet {
		fmt.Printf("applied %s provider with %d model(s) to %s\n", opts.providerID, len(models), opts.config)
		if defaultModel != "" && !opts.keepDefault {
			fmt.Printf("default model: %s/%s\n", opts.providerID, defaultModel)
		}
	}
	return 0
}

func defaultConfigPath(home string) string {
	if env := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG")); env != "" {
		return env
	}
	dir := filepath.Join(configHome(home), "opencode")
	jsonc := filepath.Join(dir, "opencode.jsonc")
	if _, err := os.Stat(jsonc); err == nil {
		return jsonc
	}
	if _, err := os.Stat(filepath.Join(dir, "opencode.json")); err == nil {
		return filepath.Join(dir, "opencode.json")
	}
	return jsonc
}

func configHome(home string) string {
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		return expandHome(v, home)
	}
	return filepath.Join(home, ".config")
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func readOptional(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []byte("{\n  \"$schema\": \"https://opencode.ai/config.json\"\n}\n"), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, true, nil
}

func loadCatalog(opts applyOptions) ([]gatewayModel, error) {
	var raw []byte
	if opts.modelsFile != "" {
		var err error
		raw, err = os.ReadFile(opts.modelsFile)
		if err != nil {
			return nil, fmt.Errorf("read model catalog: %w", err)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.modelsURL, nil)
		if err != nil {
			return nil, fmt.Errorf("model catalog URL: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "termux-proxy-rotator/"+version)
		if key, err := os.ReadFile(opts.keyFile); err == nil {
			if k := strings.TrimSpace(string(key)); k != "" {
				req.Header.Set("Authorization", "Bearer "+k)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch model catalog from %s: %w", opts.modelsURL, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("read model catalog: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			snippet := strings.TrimSpace(string(body))
			if len(snippet) > 300 {
				snippet = snippet[:300]
			}
			return nil, fmt.Errorf("model catalog returned HTTP %d: %s", resp.StatusCode, snippet)
		}
		raw = body
	}

	var catalog struct {
		Data []gatewayModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, fmt.Errorf("parse model catalog: %w", err)
	}
	seen := make(map[string]bool, len(catalog.Data))
	out := make([]gatewayModel, 0, len(catalog.Data))
	for _, m := range catalog.Data {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type gatewayModel struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	ContextLength     int64    `json:"context_length"`
	ContextWindow     int64    `json:"context_window"`
	MaxInput          int64    `json:"max_input"`
	MaxOutput         int64    `json:"max_output"`
	ToolCall          *bool    `json:"tool_call"`
	Reasoning         *bool    `json:"reasoning"`
	SupportsReasoning *bool    `json:"supports_reasoning"`
	ReasoningEfforts  []string `json:"reasoning_efforts"`
	InputModalities   []string `json:"input_modalities"`
	OutputModalities  []string `json:"output_modalities"`
	Compat            struct {
		SupportedReasoningEfforts []string `json:"supportedReasoningEfforts"`
	} `json:"compat"`
	Metadata struct {
		ContextWindow    int64    `json:"context_window"`
		MaxInput         int64    `json:"max_input"`
		MaxOutput        int64    `json:"max_output"`
		Reasoning        *bool    `json:"reasoning"`
		ToolCall         *bool    `json:"tool_call"`
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"metadata"`
}

func buildProvider(models []gatewayModel, opts applyOptions) map[string]any {
	settings := map[string]any{"baseURL": opts.baseURL}
	if opts.apiKey != "" {
		settings["apiKey"] = opts.apiKey
	}
	modelMap := make(map[string]any, len(models))
	for _, m := range models {
		entry := map[string]any{
			"name": displayName(m),
		}
		caps := map[string]any{
			"tools":  boolValue(m.ToolCall, m.Metadata.ToolCall, true),
			"input":  modalities(m.InputModalities, m.Metadata.InputModalities, []string{"text"}),
			"output": modalities(m.OutputModalities, m.Metadata.OutputModalities, []string{"text"}),
		}
		if r, known := reasoning(m); known {
			if r.supported {
				reasoningCaps := map[string]any{"supported": true}
				if len(r.efforts) > 0 {
					reasoningCaps["effort"] = r.efforts
				}
				caps["reasoning"] = reasoningCaps
			} else {
				caps["reasoning"] = map[string]any{"supported": false}
			}
		}
		entry["capabilities"] = caps

		limit := map[string]any{}
		if v := firstNonZero(m.ContextWindow, m.ContextLength, m.Metadata.ContextWindow); v > 0 {
			limit["context"] = v
		}
		if v := firstNonZero(m.MaxInput, m.Metadata.MaxInput); v > 0 {
			limit["input"] = v
		}
		if v := firstNonZero(m.MaxOutput, m.Metadata.MaxOutput); v > 0 {
			limit["output"] = v
		}
		if len(limit) > 0 {
			entry["limit"] = limit
		}
		modelMap[m.ID] = entry
	}
	return map[string]any{
		"name":     opts.name,
		"package":  "@opencode/ai/providers/openai-compatible",
		"settings": settings,
		"models":   modelMap,
	}
}

func displayName(m gatewayModel) string {
	if s := strings.TrimSpace(m.Name); s != "" {
		return s
	}
	parts := strings.Fields(strings.ReplaceAll(m.ID, "-", " "))
	for i, p := range parts {
		if p == "" {
			continue
		}
		r, size := utf8.DecodeRuneInString(p)
		parts[i] = string(unicode.ToUpper(r)) + p[size:]
	}
	return strings.Join(parts, " ")
}

func modalities(primary, fallback, def []string) []string {
	for _, list := range [][]string{primary, fallback} {
		out := cleanStrings(list)
		if len(out) > 0 {
			return out
		}
	}
	return def
}

func cleanStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func boolValue(primary, fallback *bool, def bool) bool {
	if primary != nil {
		return *primary
	}
	if fallback != nil {
		return *fallback
	}
	return def
}

func firstNonZero(vs ...int64) int64 {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}

type reasoningInfo struct {
	supported bool
	efforts   []string
}

func reasoning(m gatewayModel) (reasoningInfo, bool) {
	efforts := cleanStrings(m.ReasoningEfforts)
	if len(efforts) == 0 {
		efforts = cleanStrings(m.Compat.SupportedReasoningEfforts)
	}
	if m.Reasoning != nil {
		return reasoningInfo{supported: *m.Reasoning, efforts: efforts}, true
	}
	if m.SupportsReasoning != nil {
		return reasoningInfo{supported: *m.SupportsReasoning, efforts: efforts}, true
	}
	if m.Metadata.Reasoning != nil {
		return reasoningInfo{supported: *m.Metadata.Reasoning, efforts: efforts}, true
	}
	if len(efforts) > 0 {
		return reasoningInfo{supported: true, efforts: efforts}, true
	}
	return reasoningInfo{}, false
}

func chooseDefault(models []gatewayModel, existing string, opts applyOptions) (string, error) {
	if opts.keepDefault {
		return "", nil
	}
	has := func(id string) bool {
		for _, m := range models {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if opts.defaultModel != "" {
		id := strings.TrimPrefix(opts.defaultModel, opts.providerID+"/")
		if !has(id) {
			return "", fmt.Errorf("default model %q is not in the catalog", opts.defaultModel)
		}
		return id, nil
	}
	if strings.HasPrefix(existing, opts.providerID+"/") {
		if id := strings.TrimPrefix(existing, opts.providerID+"/"); has(id) {
			return id, nil
		}
	}
	if opts.preferModel != "" {
		id := strings.TrimPrefix(opts.preferModel, opts.providerID+"/")
		if has(id) {
			return id, nil
		}
	}
	if len(models) > 0 {
		return models[0].ID, nil
	}
	return "", fmt.Errorf("the model catalog is empty")
}

func verify(out []byte, providerID string, modelCount int, defaultModel string, keepDefault bool) error {
	doc, err := opencodecfg.Parse(out)
	if err != nil {
		return err
	}
	providers := doc.Root.Member("providers")
	if providers == nil || providers.Value.Kind != opencodecfg.ObjectKind {
		return fmt.Errorf("providers object is missing")
	}
	p := providers.Value.Member(providerID)
	if p == nil || p.Value.Kind != opencodecfg.ObjectKind {
		return fmt.Errorf("provider %q is missing", providerID)
	}
	models := p.Value.Member("models")
	if models == nil || models.Value.Kind != opencodecfg.ObjectKind {
		return fmt.Errorf("provider %q has no models object", providerID)
	}
	if len(models.Value.Members) != modelCount {
		return fmt.Errorf("provider %q has %d models, want %d", providerID, len(models.Value.Members), modelCount)
	}
	if defaultModel != "" && !keepDefault {
		got, ok := opencodecfg.FindString(doc, "model")
		if !ok {
			return fmt.Errorf("top-level model is missing")
		}
		want := providerID + "/" + defaultModel
		if got != want {
			return fmt.Errorf("top-level model is %q, want %q", got, want)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	mode := os.FileMode(0o600)
	if err == nil {
		mode = info.Mode().Perm()
	}
	return writeFileAtomicMode(dst, raw, mode)
}

func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return writeFileAtomicMode(path, data, mode)
}

func writeFileAtomicMode(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// --- detect --------------------------------------------------------------

func runDetect(args []string) int {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	home := fs.String("home", "", "home directory to search")
	gateway := fs.String("opencode2api-config", "", "explicit opencode2api config path")
	auth := fs.String("auth-file", "", "explicit OpenCode auth.json path")
	out := fs.String("out", "", "write shell assignments to this file (mode 600)")
	export := fs.Bool("export", true, "prefix assignments with export")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "opencode-apply: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *home == "" {
		*home, _ = os.UserHomeDir()
	}
	res := detect.Detect(detect.Options{
		Home:               *home,
		Opencode2APIConfig: *gateway,
		ZenAuthFile:        *auth,
	})
	var vars []envVar
	if res.Opencode2APIConfig != "" {
		vars = append(vars, envVar{"DETECTED_OPENCODE2API_CONFIG", res.Opencode2APIConfig})
	}
	if res.Opencode2APIKey != "" {
		vars = append(vars, envVar{"DETECTED_OPENCODE2API_KEY", res.Opencode2APIKey})
	}
	if res.Opencode2APIURL != "" {
		vars = append(vars, envVar{"DETECTED_OPENCODE2API_URL", res.Opencode2APIURL})
	}
	if res.ZenAuthFile != "" {
		vars = append(vars, envVar{"DETECTED_OPENCODE_ZEN_AUTH_FILE", res.ZenAuthFile})
	}
	if res.ZenKey != "" {
		vars = append(vars, envVar{"DETECTED_OPENCODE_ZEN_KEY", res.ZenKey})
	}
	var b strings.Builder
	for _, v := range vars {
		if *export {
			b.WriteString("export ")
		}
		b.WriteString(v.name)
		b.WriteString("=")
		b.WriteString(shellQuote(v.value))
		b.WriteString("\n")
	}
	if *out != "" {
		if err := writeFileAtomicMode(*out, []byte(b.String()), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "opencode-apply: write %s: %v\n", *out, err)
			return 1
		}
	} else {
		_, _ = io.WriteString(os.Stdout, b.String())
	}
	return 0
}

type envVar struct {
	name  string
	value string
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
