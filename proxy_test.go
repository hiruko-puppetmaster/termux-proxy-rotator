package main

import (
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testSettings(upstreams ...*Upstream) *Settings {
	return &Settings{
		Listen:          "127.0.0.1:0",
		MaxAttempts:     6,
		MaxRequestBytes: 1 << 20,
		HeaderTimeout:   2 * time.Second,
		MaxCooldown:     10 * time.Minute,
		LogLevel:        LevelQuiet,
		RetryStatuses:   map[int]bool{429: true, 403: true, 502: true, 503: true},
		Cooldowns: map[int]time.Duration{
			429: 30 * time.Second,
			403: 120 * time.Second,
			502: 10 * time.Second,
			503: 15 * time.Second,
		},
		NetworkCooldown: 10 * time.Second,
		DefaultCooldown: 15 * time.Second,
		Upstreams:       upstreams,
	}
}

func testUpstream(t *testing.T, name, rawURL, key string, priority int) *Upstream {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	return &Upstream{
		Name:       name,
		Base:       u,
		APIKey:     key,
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		Priority:   priority,
		Headers:    http.Header{},
		Enabled:    true,
	}
}

func newProxyServer(t *testing.T, cfg *Settings) (*Proxy, *httptest.Server) {
	t.Helper()
	p := NewProxy(cfg, NewLogger(io.Discard, LevelQuiet))
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return p, srv
}

func doJSONRequest(t *testing.T, client *http.Client, endpoint, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

func TestTargetURL(t *testing.T) {
	tests := []struct {
		base, path, want string
	}{
		{"http://127.0.0.1:8080/v1", "/v1/chat/completions", "http://127.0.0.1:8080/v1/chat/completions"},
		{"http://127.0.0.1:8080/v1", "/chat/completions", "http://127.0.0.1:8080/v1/chat/completions"},
		{"http://127.0.0.1:8080", "/v1/models", "http://127.0.0.1:8080/v1/models"},
		{"https://api.groq.com/openai/v1", "/v1/chat/completions", "https://api.groq.com/openai/v1/chat/completions"},
		{"http://example.com/api", "/v1/models", "http://example.com/api/v1/models"},
		{"http://example.com/v1", "/v1", "http://example.com/v1"},
		{"http://example.com/v1?base=1", "/v1/models?q=2", "http://example.com/v1/models?base=1&q=2"},
	}
	for _, tc := range tests {
		u, err := url.Parse(tc.base)
		if err != nil {
			t.Fatal(err)
		}
		up := &Upstream{Base: u}
		inPath, inQuery, _ := strings.Cut(tc.path, "?")
		if got := up.TargetURL(inPath, inQuery); got != tc.want {
			t.Errorf("TargetURL(%s, %s) = %s, want %s", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestPoolPriorityCooldownAndForcedAttempt(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testUpstream(t, "a", "http://a.example/v1", "a", 1)
	b := testUpstream(t, "b", "http://b.example/v1", "b", 0)
	cfg := testSettings(a, b)
	pool := NewPool(cfg)
	pool.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		pick, ok := pool.Pick(nil, true)
		if !ok || pick.index != 1 || pick.forced {
			t.Fatalf("pick %d = %+v, %v; want healthy b", i, pick, ok)
		}
	}

	if cd := pool.Fail(1, http.StatusTooManyRequests, 0, "rate limited"); cd != 30*time.Second {
		t.Fatalf("429 cooldown = %s, want 30s", cd)
	}
	pick, ok := pool.Pick(nil, true)
	if !ok || pick.index != 0 || pick.forced {
		t.Fatalf("after cooling b: pick = %+v, %v; want a", pick, ok)
	}

	snap := pool.Snapshot(now)
	if snap[1].Healthy || snap[1].CooldownRemainingSeconds < 29 || snap[1].LastStatus != 429 {
		t.Fatalf("unexpected b status: %+v", snap[1])
	}

	if cd := pool.Fail(0, http.StatusServiceUnavailable, 0, "gateway down"); cd != 15*time.Second {
		t.Fatalf("503 cooldown = %s, want 15s", cd)
	}
	pick, ok = pool.Pick(nil, true)
	if !ok || !pick.forced || pick.index != 0 {
		t.Fatalf("all cooling: pick = %+v, %v; want forced a (recovers first)", pick, ok)
	}
	if _, ok := pool.Pick(nil, false); ok {
		t.Fatal("forced pick allowed when allowForced=false")
	}

	now = now.Add(31 * time.Second)
	pick, ok = pool.Pick(nil, true)
	if !ok || pick.index != 1 || pick.forced {
		t.Fatalf("after cooldown: pick = %+v, %v; want healthy b", pick, ok)
	}
	pool.Success(1, http.StatusOK, 5*time.Millisecond)
	snap = pool.Snapshot(now)
	if !snap[1].Healthy || snap[1].ConsecutiveFailures != 0 {
		t.Fatalf("success did not clear b health: %+v", snap[1])
	}
}

func TestPoolRoundRobinWithinPriority(t *testing.T) {
	a := testUpstream(t, "a", "http://a.example/v1", "a", 0)
	b := testUpstream(t, "b", "http://b.example/v1", "b", 0)
	low := testUpstream(t, "low", "http://low.example/v1", "low", 10)
	pool := NewPool(testSettings(a, b, low))

	want := []int{0, 1, 0, 1}
	for i, idx := range want {
		pick, ok := pool.Pick(nil, true)
		if !ok || pick.index != idx || pick.forced {
			t.Fatalf("pick %d = %+v, %v; want index %d", i, pick, ok, idx)
		}
	}
}

func TestPoolHandlesMaxIntPriority(t *testing.T) {
	a := testUpstream(t, "max-priority", "http://a.example/v1", "a", math.MaxInt)
	pool := NewPool(testSettings(a))
	pick, ok := pool.Pick(nil, true)
	if !ok || pick.index != 0 || pick.forced {
		t.Fatalf("pick = %+v, %v; want healthy upstream with MaxInt priority", pick, ok)
	}
}

func TestCooldownRetryAfterAndBackoff(t *testing.T) {
	a := testUpstream(t, "a", "http://a.example/v1", "a", 0)
	pool := NewPool(testSettings(a))
	fixed := time.Now()
	pool.now = func() time.Time { return fixed }

	if cd := pool.Fail(0, http.StatusTooManyRequests, 7*time.Second, "slow down"); cd != 7*time.Second {
		t.Fatalf("Retry-After cooldown = %s, want 7s", cd)
	}
	pool.ResetCooldowns()

	if cd := pool.Fail(0, http.StatusTooManyRequests, 0, "limited"); cd != 30*time.Second {
		t.Fatalf("first 429 cooldown = %s, want 30s", cd)
	}
	if cd := pool.Fail(0, http.StatusTooManyRequests, 0, "limited again"); cd != 60*time.Second {
		t.Fatalf("second 429 cooldown = %s, want 60s (exponential)", cd)
	}
}

func TestRetryStatusFailover(t *testing.T) {
	for _, status := range []int{403, 502, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var aHits, bHits atomic.Int32
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				aHits.Add(1)
				http.Error(w, "provider rejected request", status)
			}))
			defer a.Close()
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer b.Close()

			cfg := testSettings(
				testUpstream(t, "a", a.URL+"/v1", "key-a", 0),
				testUpstream(t, "b", b.URL+"/v1", "key-b", 1),
			)
			p, srv := newProxyServer(t, cfg)
			client := &http.Client{Timeout: 5 * time.Second}
			resp := doJSONRequest(t, client, srv.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
				t.Fatalf("response = %d %q, want 200 from b", resp.StatusCode, body)
			}
			if aHits.Load() != 1 || bHits.Load() != 1 {
				t.Fatalf("hits a=%d b=%d, want 1/1", aHits.Load(), bHits.Load())
			}
			if got := resp.Header.Get("X-Rotator-Upstream"); got != "b" {
				t.Fatalf("X-Rotator-Upstream = %q, want b", got)
			}
			if p.failovers.Load() != 1 {
				t.Fatalf("failovers = %d, want 1", p.failovers.Load())
			}
			snap := p.pool.Snapshot(time.Now())
			if snap[0].Healthy || snap[0].LastStatus != status {
				t.Fatalf("a not cooled with status %d: %+v", status, snap[0])
			}
		})
	}
}

func TestNonRetryablePassthrough(t *testing.T) {
	for _, status := range []int{401, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var aHits, bHits atomic.Int32
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				aHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"from a"}}`)
			}))
			defer a.Close()
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bHits.Add(1)
				_, _ = io.WriteString(w, "unexpected")
			}))
			defer b.Close()

			p, srv := newProxyServer(t, testSettings(
				testUpstream(t, "a", a.URL+"/v1", "key-a", 0),
				testUpstream(t, "b", b.URL+"/v1", "key-b", 1),
			))
			resp := doJSONRequest(t, &http.Client{Timeout: 5 * time.Second}, srv.URL+"/v1/chat/completions", `{}`, nil)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, status)
			}
			if !strings.Contains(string(body), "from a") {
				t.Fatalf("body = %q, want upstream error", body)
			}
			if aHits.Load() != 1 || bHits.Load() != 0 {
				t.Fatalf("hits a=%d b=%d, want 1/0 (no failover)", aHits.Load(), bHits.Load())
			}
			if p.failovers.Load() != 0 {
				t.Fatalf("failovers = %d, want 0", p.failovers.Load())
			}
			if status == 500 {
				if snap := p.pool.Snapshot(time.Now()); snap[0].Healthy {
					t.Fatal("a should be cooled after a passed-through 500")
				}
			}
		})
	}
}

func TestExhaustedPoolReturns429(t *testing.T) {
	var hits atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer a.Close()

	p, srv := newProxyServer(t, testSettings(testUpstream(t, "a", a.URL+"/v1", "key-a", 0)))
	resp := doJSONRequest(t, &http.Client{Timeout: 5 * time.Second}, srv.URL+"/v1/chat/completions", `{"model":"m","stream":true}`, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want 3", got)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error.Code != "upstream_exhausted" {
		t.Fatalf("unexpected error body %q (err %v)", body, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (single upstream, no duplicate attempts)", hits.Load())
	}
	if p.failovers.Load() != 1 {
		t.Fatalf("failovers = %d, want 1", p.failovers.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Fatalf("parseRetryAfter(2) = %s", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Fatalf("parseRetryAfter(garbage) = %s, want 0", got)
	}
	future := time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 2*time.Second || got > 5*time.Second {
		t.Fatalf("parseRetryAfter(date) = %s, want about 4s", got)
	}
}

func TestNetworkErrorFailover(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	var bHits atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer b.Close()

	cfg := testSettings(
		testUpstream(t, "dead", "http://"+deadAddr+"/v1", "key-a", 0),
		testUpstream(t, "b", b.URL+"/v1", "key-b", 1),
	)
	p, srv := newProxyServer(t, cfg)
	resp := doJSONRequest(t, &http.Client{Timeout: 5 * time.Second}, srv.URL+"/v1/chat/completions", `{}`, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("response = %d %q, want 200 from b", resp.StatusCode, body)
	}
	if bHits.Load() != 1 || p.failovers.Load() != 1 {
		t.Fatalf("b hits = %d, failovers = %d; want 1/1", bHits.Load(), p.failovers.Load())
	}
	snap := p.pool.Snapshot(time.Now())
	if snap[0].Healthy || snap[0].LastError == "" {
		t.Fatalf("dead upstream not marked unhealthy: %+v", snap[0])
	}
}

func TestAdminStatusAndReset(t *testing.T) {
	a := testUpstream(t, "a", "http://a.example/v1", "key-a", 0)
	p, _ := newProxyServer(t, testSettings(a))
	p.pool.Fail(0, http.StatusTooManyRequests, 0, "rate limited")

	admin := httptest.NewServer(p.AdminHandler())
	defer admin.Close()
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(admin.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status adminStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(status.Upstreams) != 1 {
		t.Fatalf("status = %d, upstreams=%d", resp.StatusCode, len(status.Upstreams))
	}
	if status.Upstreams[0].Healthy {
		t.Fatalf("upstream should be cooling: %+v", status.Upstreams[0])
	}

	req, _ := http.NewRequest(http.MethodPost, admin.URL+"/reset", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset status = %d", resp.StatusCode)
	}
	if snap := p.pool.Snapshot(time.Now()); !snap[0].Healthy {
		t.Fatalf("reset did not clear cooldown: %+v", snap[0])
	}

	resp, err = client.Post(admin.URL+"/status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /status = %d, want 405", resp.StatusCode)
	}
}
