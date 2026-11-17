package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the on-disk JSON format. Missing fields keep their defaults.
type Config struct {
	Listen                       string           `json:"listen"`
	AdminListen                  string           `json:"admin_listen"`
	MaxAttempts                  int              `json:"max_attempts"`
	MaxRequestBytes              int64            `json:"max_request_bytes"`
	UpstreamHeaderTimeoutSeconds int              `json:"upstream_header_timeout_seconds"`
	MaxCooldownSeconds           int              `json:"max_cooldown_seconds"`
	ClientAPIKey                 string           `json:"client_api_key"`
	LogLevel                     string           `json:"log_level"`
	RetryStatuses                []int            `json:"retry_statuses"`
	CooldownSeconds              map[string]int   `json:"cooldown_seconds"`
	Upstreams                    []UpstreamConfig `json:"upstreams"`
}

type UpstreamConfig struct {
	Name       string            `json:"name"`
	BaseURL    string            `json:"base_url"`
	APIKey     string            `json:"api_key"`
	AuthHeader string            `json:"auth_header"`
	AuthPrefix *string           `json:"auth_prefix"`
	Priority   int               `json:"priority"`
	Enabled    *bool             `json:"enabled"`
	Headers    map[string]string `json:"headers"`
}

// Settings is the validated, runtime-ready configuration.
type Settings struct {
	Listen          string
	AdminListen     string
	MaxAttempts     int
	MaxRequestBytes int64
	HeaderTimeout   time.Duration
	MaxCooldown     time.Duration
	ClientAPIKey    string
	LogLevel        LogLevel
	RetryStatuses   map[int]bool
	Cooldowns       map[int]time.Duration
	NetworkCooldown time.Duration
	DefaultCooldown time.Duration
	Upstreams       []*Upstream
}

func (s *Settings) retryable(status int) bool {
	return s.RetryStatuses[status]
}

// Upstream is one credential/endpoint pair. Mutable health fields are guarded
// by Pool.mu.
type Upstream struct {
	Name       string
	Base       *url.URL
	APIKey     string
	AuthHeader string
	AuthPrefix string
	Priority   int
	Headers    http.Header
	Enabled    bool

	cooldownUntil       time.Time
	consecutiveFailures int
	attempts            int64
	successes           int64
	failures            int64
	lastError           string
	lastStatus          int
	lastUsed            time.Time
	lastLatency         time.Duration
}

func defaultConfig() *Config {
	return &Config{
		Listen:                       "127.0.0.1:9090",
		MaxAttempts:                  6,
		MaxRequestBytes:              32 << 20,
		UpstreamHeaderTimeoutSeconds: 120,
		MaxCooldownSeconds:           600,
		LogLevel:                     "info",
		RetryStatuses:                []int{429, 403, 502, 503},
		CooldownSeconds: map[string]int{
			"429": 30, "403": 120, "502": 10, "503": 15, "network": 10, "default": 15,
		},
	}
}

func LoadConfig(path string) (*Settings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := defaultConfig()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("parse %s: unexpected data after JSON object", path)
	}
	return buildSettings(cfg)
}

func buildSettings(c *Config) (*Settings, error) {
	c.Listen = os.ExpandEnv(strings.TrimSpace(c.Listen))
	c.AdminListen = os.ExpandEnv(strings.TrimSpace(c.AdminListen))
	c.ClientAPIKey = os.ExpandEnv(strings.TrimSpace(c.ClientAPIKey))
	if strings.ContainsAny(c.ClientAPIKey, "\r\n") {
		return nil, fmt.Errorf("client_api_key must not contain CR/LF")
	}

	if c.Listen == "" {
		c.Listen = "127.0.0.1:9090"
	}
	if err := validateListenAddr(c.Listen); err != nil {
		return nil, err
	}
	if c.AdminListen != "" {
		if err := validateListenAddr(c.AdminListen); err != nil {
			return nil, fmt.Errorf("admin_listen: %w", err)
		}
	}

	if c.MaxAttempts < 0 {
		return nil, fmt.Errorf("max_attempts must be >= 0 (0 = try every upstream)")
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = 32 << 20
	}
	if c.MaxRequestBytes > 1<<30 {
		return nil, fmt.Errorf("max_request_bytes is unreasonably large (max 1 GiB)")
	}
	if c.UpstreamHeaderTimeoutSeconds <= 0 {
		c.UpstreamHeaderTimeoutSeconds = 120
	}
	if c.UpstreamHeaderTimeoutSeconds > 3600 {
		return nil, fmt.Errorf("upstream_header_timeout_seconds must be <= 3600")
	}
	if c.MaxCooldownSeconds < 1 || c.MaxCooldownSeconds > 86400 {
		return nil, fmt.Errorf("max_cooldown_seconds must be between 1 and 86400")
	}

	level, err := parseLogLevel(c.LogLevel)
	if err != nil {
		return nil, err
	}

	retry := make(map[int]bool)
	for _, status := range c.RetryStatuses {
		if status < 400 || status > 599 {
			return nil, fmt.Errorf("retry_statuses: %d is not an HTTP error status (400-599)", status)
		}
		retry[status] = true
	}

	cooldowns := make(map[int]time.Duration)
	networkCooldown := 10 * time.Second
	defaultCooldown := 15 * time.Second
	for k, seconds := range c.CooldownSeconds {
		k = strings.TrimSpace(k)
		if seconds < 1 || seconds > 86400 {
			return nil, fmt.Errorf("cooldown_seconds[%q] must be between 1 and 86400", k)
		}
		d := time.Duration(seconds) * time.Second
		switch strings.ToLower(k) {
		case "network":
			networkCooldown = d
		case "default":
			defaultCooldown = d
		default:
			status, err := strconv.Atoi(k)
			if err != nil || status < 400 || status > 599 {
				return nil, fmt.Errorf("cooldown_seconds: invalid key %q (want an HTTP status, \"network\" or \"default\")", k)
			}
			cooldowns[status] = d
		}
	}

	upstreams, err := buildUpstreams(c.Upstreams)
	if err != nil {
		return nil, err
	}

	return &Settings{
		Listen:          c.Listen,
		AdminListen:     c.AdminListen,
		MaxAttempts:     c.MaxAttempts,
		MaxRequestBytes: c.MaxRequestBytes,
		HeaderTimeout:   time.Duration(c.UpstreamHeaderTimeoutSeconds) * time.Second,
		MaxCooldown:     time.Duration(c.MaxCooldownSeconds) * time.Second,
		ClientAPIKey:    c.ClientAPIKey,
		LogLevel:        level,
		RetryStatuses:   retry,
		Cooldowns:       cooldowns,
		NetworkCooldown: networkCooldown,
		DefaultCooldown: defaultCooldown,
		Upstreams:       upstreams,
	}, nil
}

func buildUpstreams(in []UpstreamConfig) ([]*Upstream, error) {
	out := make([]*Upstream, 0, len(in))
	usedNames := make(map[string]bool)

	for i, uc := range in {
		if uc.Enabled != nil && !*uc.Enabled {
			continue
		}
		rawBase := strings.TrimSpace(os.ExpandEnv(uc.BaseURL))
		if rawBase == "" {
			return nil, fmt.Errorf("upstreams[%d]: base_url is required", i)
		}
		base, err := url.Parse(rawBase)
		if err != nil {
			return nil, fmt.Errorf("upstreams[%d]: invalid base_url: %w", i, err)
		}
		base.Scheme = strings.ToLower(base.Scheme)
		if base.Scheme != "http" && base.Scheme != "https" {
			return nil, fmt.Errorf("upstreams[%d]: base_url must use http or https", i)
		}
		if base.Host == "" {
			return nil, fmt.Errorf("upstreams[%d]: base_url has no host", i)
		}
		if base.Fragment != "" {
			return nil, fmt.Errorf("upstreams[%d]: base_url must not contain a fragment", i)
		}
		base.Path = strings.TrimSuffix(base.Path, "/")
		if base.Path == "/" {
			base.Path = ""
		}

		name := strings.TrimSpace(uc.Name)
		if name == "" {
			name = fmt.Sprintf("upstream-%d", len(out)+1)
		}
		unique := name
		for n := 2; usedNames[unique]; n++ {
			unique = fmt.Sprintf("%s-%d", name, n)
		}
		if !validUpstreamName(unique) {
			return nil, fmt.Errorf("upstreams[%d]: name must be at most 64 printable characters", i)
		}
		usedNames[unique] = true

		key := strings.TrimSpace(os.ExpandEnv(uc.APIKey))
		if strings.ContainsAny(key, "\r\n") {
			return nil, fmt.Errorf("upstream %q: api_key must not contain CR/LF", unique)
		}
		authHeader := strings.TrimSpace(uc.AuthHeader)
		if authHeader == "" {
			authHeader = "Authorization"
		}
		if !validHeaderName(authHeader) {
			return nil, fmt.Errorf("upstream %q: invalid auth_header %q", unique, authHeader)
		}
		prefix := "Bearer "
		if uc.AuthPrefix != nil {
			prefix = os.ExpandEnv(*uc.AuthPrefix)
		}
		if strings.ContainsAny(prefix, "\r\n") {
			return nil, fmt.Errorf("upstream %q: auth_prefix must not contain CR/LF", unique)
		}

		headers := make(http.Header, len(uc.Headers))
		for hk, hv := range uc.Headers {
			hk = strings.TrimSpace(hk)
			hv = os.ExpandEnv(hv)
			if !validHeaderName(hk) {
				return nil, fmt.Errorf("upstream %q: invalid header name %q", unique, hk)
			}
			if strings.ContainsAny(hv, "\r\n") {
				return nil, fmt.Errorf("upstream %q: header %q contains CR/LF", unique, hk)
			}
			switch http.CanonicalHeaderKey(hk) {
			case "Host", "Content-Length", "Transfer-Encoding", "Connection", "Keep-Alive",
				"Proxy-Connection", "Upgrade", "Te", "Trailer", "Accept-Encoding":
				return nil, fmt.Errorf("upstream %q: header %q cannot be overridden", unique, hk)
			}
			headers.Set(hk, hv)
		}

		out = append(out, &Upstream{
			Name:       unique,
			Base:       base,
			APIKey:     key,
			AuthHeader: authHeader,
			AuthPrefix: prefix,
			Priority:   uc.Priority,
			Headers:    headers,
			Enabled:    true,
		})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no enabled upstreams configured")
	}
	return out, nil
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid listen address %q: port must be 1-65535", addr)
	}
	return nil
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func validUpstreamName(s string) bool {
	r := []rune(s)
	if len(r) == 0 || len(r) > 64 {
		return false
	}
	for _, c := range r {
		if c < 0x20 || (c >= 0x7f && c <= 0x9f) {
			return false
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

// TargetURL joins an inbound API path with the upstream base URL. If both end
// and start with /v1 respectively, the duplicate is removed so that clients
// using base http://localhost:9090/v1 work with upstreams using .../v1 too.
func (u *Upstream) TargetURL(inPath, inRawQuery string) string {
	target := *u.Base
	basePath := strings.TrimSuffix(target.Path, "/")
	if inPath == "" {
		inPath = "/"
	}
	if !strings.HasPrefix(inPath, "/") {
		inPath = "/" + inPath
	}
	if strings.HasSuffix(basePath, "/v1") && (inPath == "/v1" || strings.HasPrefix(inPath, "/v1/")) {
		inPath = strings.TrimPrefix(inPath, "/v1")
	}
	target.Path = basePath + inPath
	if target.Path == "" {
		target.Path = "/"
	}
	target.RawPath = ""
	target.Fragment = ""
	switch {
	case target.RawQuery == "":
		target.RawQuery = inRawQuery
	case inRawQuery == "":
		// keep the base query
	default:
		target.RawQuery += "&" + inRawQuery
	}
	return target.String()
}

// displayURL omits userinfo and query strings so credentials never reach logs
// or the admin status endpoint.
func (u *Upstream) displayURL() string {
	return u.Base.Scheme + "://" + u.Base.Host + u.Base.Path
}
