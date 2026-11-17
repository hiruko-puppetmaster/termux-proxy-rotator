package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func recvString(t *testing.T, ch <-chan string, label string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return ""
	}
}

func TestStreamingFailoverPreservesChunks(t *testing.T) {
	requestBody := `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	wantSSE := "data: first\n\ndata: second\n\n"

	aBody := make(chan string, 4)
	aAuth := make(chan string, 4)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		aBody <- string(b)
		aAuth <- r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream a path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited by a"}}`)
	}))
	defer a.Close()

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseB := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseB()

	bBody := make(chan string, 4)
	bAuth := make(chan string, 4)
	var bHits atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		bBody <- string(body)
		bAuth <- r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream b path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream b lacks flusher")
			return
		}
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: second\n\n")
		flusher.Flush()
	}))
	defer b.Close()

	cfg := testSettings(
		testUpstream(t, "a", a.URL+"/v1", "key-a", 0),
		testUpstream(t, "b", b.URL+"/v1", "key-b", 1),
	)
	p, srv := newProxyServer(t, cfg)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-dummy")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Rotator-Upstream"); got != "b" {
		t.Fatalf("X-Rotator-Upstream = %q, want b", got)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}

	// b holds the second event until we close release. Receiving the first
	// event before that proves the proxy flushes SSE data immediately.
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first SSE line: %v", err)
	}
	if first != "data: first\n" {
		t.Fatalf("first SSE line = %q", first)
	}
	releaseB()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read rest of stream: %v", err)
	}
	if got := first + string(rest); got != wantSSE {
		t.Fatalf("stream body = %q, want %q", got, wantSSE)
	}

	if got := recvString(t, aBody, "a body"); got != requestBody {
		t.Fatalf("upstream a body = %q", got)
	}
	if got := recvString(t, bBody, "b body"); got != requestBody {
		t.Fatalf("upstream b body = %q (body was not replayed)", got)
	}
	if got := recvString(t, aAuth, "a auth"); got != "Bearer key-a" {
		t.Fatalf("upstream a auth = %q", got)
	}
	if got := recvString(t, bAuth, "b auth"); got != "Bearer key-b" {
		t.Fatalf("upstream b auth = %q", got)
	}
	if bHits.Load() != 1 {
		t.Fatalf("b hits = %d, want 1", bHits.Load())
	}
	if p.failovers.Load() != 1 {
		t.Fatalf("failovers = %d, want 1", p.failovers.Load())
	}
	snap := p.pool.Snapshot(time.Now())
	if snap[0].Healthy || snap[0].LastStatus != http.StatusTooManyRequests {
		t.Fatalf("upstream a should be cooling after 429: %+v", snap[0])
	}
	if !snap[1].Healthy {
		t.Fatalf("upstream b should be healthy: %+v", snap[1])
	}
}

func TestClientAuthAndProxyTokenIsNotLeaked(t *testing.T) {
	aAuth := make(chan string, 4)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer a.Close()

	bAuth := make(chan string, 4)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer b.Close()

	cfg := testSettings(
		testUpstream(t, "a", a.URL+"/v1", "", 0),
		testUpstream(t, "b", b.URL+"/v1", "", 1),
	)
	cfg.ClientAPIKey = "proxy-secret"
	_, srv := newProxyServer(t, cfg)

	client := &http.Client{Timeout: 5 * time.Second}
	resp := doJSONRequest(t, client, srv.URL+"/v1/chat/completions", `{}`, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	resp = doJSONRequest(t, client, srv.URL+"/v1/chat/completions", `{}`, map[string]string{
		"Authorization": "Bearer proxy-secret",
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("authenticated response = %d %q", resp.StatusCode, body)
	}
	if got := recvString(t, aAuth, "a auth"); got != "" {
		t.Fatalf("proxy token leaked to a: %q", got)
	}
	if got := recvString(t, bAuth, "b auth"); got != "" {
		t.Fatalf("proxy token leaked to b: %q", got)
	}
}
