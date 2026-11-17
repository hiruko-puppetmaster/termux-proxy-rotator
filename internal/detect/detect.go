// Package detect finds local upstream credentials that a Termux OpenCode
// installation can use: the opencode2api gateway and the OpenCode Zen key.
package detect

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"proxyrotator/internal/opencodecfg"
)

// Result describes the upstreams that were found. Empty fields mean the
// corresponding upstream was not detected.
type Result struct {
	Opencode2APIConfig string
	Opencode2APIKey    string
	Opencode2APIURL    string
	ZenAuthFile        string
	ZenKey             string
}

// Options controls detection. All fields are optional.
type Options struct {
	Home string
	// Opencode2APIConfig is checked before the default candidates.
	Opencode2APIConfig string
	// ZenAuthFile is checked before the default candidates.
	ZenAuthFile string
	// ExtraPaths are additional opencode2api config candidates.
	ExtraPaths []string
}

// Detect searches for local upstream configuration.
func Detect(opts Options) Result {
	home := opts.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	res := Result{}

	candidates := make([]string, 0, 8)
	if opts.Opencode2APIConfig != "" {
		candidates = append(candidates, opts.Opencode2APIConfig)
	}
	if env := strings.TrimSpace(os.Getenv("OPENCODE2API_CONFIG")); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates, opts.ExtraPaths...)
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, "opencode2api", "config.json"),
			filepath.Join(home, ".config", "opencode2api", "config.json"),
			filepath.Join(home, ".local", "share", "opencode2api", "config.json"),
		)
	}
	// Termux's real home is sometimes different from $HOME when the installer
	// runs under a root shell (for example in a proot container).
	candidates = append(candidates, "/data/data/com.termux/files/home/opencode2api/config.json")

	for _, path := range uniquePaths(candidates) {
		cfg, err := readOne(path)
		if err != nil {
			continue
		}
		res.Opencode2APIConfig = path
		res.Opencode2APIKey = cfg.key
		res.Opencode2APIURL = cfg.url
		break
	}

	authCandidates := make([]string, 0, 5)
	if opts.ZenAuthFile != "" {
		authCandidates = append(authCandidates, opts.ZenAuthFile)
	}
	if env := strings.TrimSpace(os.Getenv("OPENCODE_ZEN_AUTH_FILE")); env != "" {
		authCandidates = append(authCandidates, env)
	}
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		authCandidates = append(authCandidates, filepath.Join(dataHome, "opencode", "auth.json"))
	}
	if home != "" {
		authCandidates = append(authCandidates, filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	}
	authCandidates = append(authCandidates, "/data/data/com.termux/files/home/.local/share/opencode/auth.json")

	for _, path := range uniquePaths(authCandidates) {
		key, ok := readZenKey(path)
		if !ok {
			continue
		}
		res.ZenAuthFile = path
		res.ZenKey = key
		break
	}

	return res
}

type gatewayConfig struct {
	key string
	url string
}

func readOne(path string) (gatewayConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return gatewayConfig{}, err
	}
	doc, err := opencodecfg.Parse(raw)
	if err != nil {
		return gatewayConfig{}, err
	}
	key := firstStringArrayValue(doc.Root, "server_keys")
	if key == "" {
		return gatewayConfig{}, fmt.Errorf("no server_keys")
	}
	listen := ""
	if m := doc.Root.Member("listen"); m != nil && m.Value.Kind == opencodecfg.StringKind {
		listen = m.Value.Str
	}
	return gatewayConfig{key: key, url: gatewayURL(listen)}, nil
}

func readZenKey(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	doc, err := opencodecfg.Parse(raw)
	if err != nil {
		return "", false
	}
	key, ok := opencodecfg.FindString(doc, "opencode", "key")
	if !ok || strings.TrimSpace(key) == "" {
		return "", false
	}
	return key, true
}

func firstStringArrayValue(root *opencodecfg.Node, key string) string {
	m := root.Member(key)
	if m == nil || m.Value.Kind != opencodecfg.ArrayKind {
		return ""
	}
	for _, item := range m.Value.Items {
		if item.Kind == opencodecfg.StringKind && strings.TrimSpace(item.Str) != "" {
			return item.Str
		}
	}
	return ""
}

// gatewayURL turns an opencode2api listen address such as 127.0.0.1:8080 into
// a client base URL. Wildcard hosts are replaced with loopback.
func gatewayURL(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	if !strings.Contains(listen, "://") {
		listen = "http://" + listen
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(listen, "http://"), "https://"))
	scheme := "http"
	if strings.HasPrefix(listen, "https://") {
		scheme = "https"
	}
	if err != nil || port == "" {
		return "http://127.0.0.1:8080/v1"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + ":" + port + "/v1"
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
