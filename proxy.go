package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	cfg    *Settings
	pool   *Pool
	log    *Logger
	client *http.Client

	started   time.Time
	requests  atomic.Uint64
	failovers atomic.Uint64
	active    atomic.Int64
}

func NewProxy(cfg *Settings, log *Logger) *Proxy {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: cfg.HeaderTimeout,
		// Identity encoding keeps SSE chunks intact and avoids gzip buffering.
		DisableCompression: true,
		ReadBufferSize:     32 << 10,
		WriteBufferSize:    32 << 10,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Proxy{
		cfg:     cfg,
		pool:    NewPool(cfg),
		log:     log,
		client:  client,
		started: time.Now(),
	}
}

func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(p.serveHTTP)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p.active.Add(1)
	defer p.active.Add(-1)
	reqID := newRequestID()
	started := time.Now()

	// Small local endpoints for probes. Reserved; not forwarded upstream.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		switch r.URL.Path {
		case "/healthz":
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "ok", "service": "proxy-rotator", "version": version,
			})
			return
		case "/":
			writeJSON(w, http.StatusOK, map[string]any{
				"service": "proxy-rotator", "version": version, "upstreams": len(p.cfg.Upstreams),
			})
			return
		}
	}
	p.requests.Add(1)

	if !p.authorized(r) {
		p.log.warnf(reqID, "rejected unauthorized request from %s", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", `Bearer realm="proxy-rotator"`)
		writeAPIError(w, http.StatusUnauthorized, "invalid_client_api_key", "invalid or missing client API key")
		return
	}

	body, err := readRequestBody(r, p.cfg.MaxRequestBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			p.log.warnf(reqID, "request body exceeds limit of %s", fmtBytes(p.cfg.MaxRequestBytes))
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("request body exceeds the proxy limit of %d bytes", p.cfg.MaxRequestBytes))
			return
		}
		p.log.warnf(reqID, "failed reading request body: %v", err)
		writeAPIError(w, http.StatusBadRequest, "invalid_request_body", "could not read request body")
		return
	}

	model, streamRequested := requestMeta(body)
	reqLine := r.Method + " " + sanitizeLog(r.URL.Path, 200)
	if model != "" {
		reqLine += " model=" + sanitizeLog(model, 80)
	}
	if streamRequested {
		reqLine += " stream=true"
	}
	p.log.requestf(reqID, "%s", reqLine)

	attempts := p.cfg.MaxAttempts
	if attempts <= 0 || attempts > len(p.cfg.Upstreams) {
		attempts = len(p.cfg.Upstreams)
	}
	if attempts < 1 {
		p.log.errorf(reqID, "no upstreams available")
		writeAPIError(w, http.StatusBadGateway, "no_upstreams", "no upstreams are configured")
		return
	}

	baseHeaders := p.forwardHeaders(r.Header)
	tried := make(map[int]struct{}, attempts)
	forcedUsed := false
	attempted := 0
	var lastFailure attemptFailure

	for attempt := 1; attempt <= attempts; attempt++ {
		if r.Context().Err() != nil {
			p.log.debugf(reqID, "client disconnected before attempt %d", attempt)
			return
		}

		pick, ok := p.pool.Pick(tried, !forcedUsed)
		if !ok {
			break
		}
		if pick.forced {
			forcedUsed = true
			up := p.cfg.Upstreams[pick.index]
			p.log.warnf(reqID, "all upstreams are cooling down; one forced attempt on %s (earliest recovery in %s)",
				up.Name, fmtDuration(pick.wait))
		}
		tried[pick.index] = struct{}{}
		attempted++
		up := p.cfg.Upstreams[pick.index]

		target := up.TargetURL(r.URL.Path, r.URL.RawQuery)
		headers := baseHeaders.Clone()
		up.applyHeaders(headers)
		upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			cooldown := p.pool.Fail(pick.index, 0, 0, "build request: "+err.Error())
			p.failovers.Add(1)
			lastFailure = attemptFailure{upstream: up.Name, detail: sanitizeLog(err.Error(), 200), cooldown: cooldown}
			p.log.failf(reqID, "%s request build error: %s; cooling %s", up.Name, sanitizeLog(err.Error(), 160), fmtDuration(cooldown))
			continue
		}
		upReq.Header = headers

		reqStart := time.Now()
		resp, err := p.client.Do(upReq)
		if err != nil {
			if r.Context().Err() != nil {
				p.log.debugf(reqID, "upstream %s request aborted by client: %v", up.Name, err)
				return
			}
			cooldown := p.pool.Fail(pick.index, 0, 0, err.Error())
			p.failovers.Add(1)
			lastFailure = attemptFailure{
				upstream: up.Name, detail: sanitizeLog(err.Error(), 200), cooldown: cooldown,
			}
			p.log.failf(reqID, "%s network error after %s (attempt %d/%d): %s; cooling %s, rotating",
				up.Name, fmtDuration(time.Since(reqStart)), attempt, attempts,
				sanitizeLog(err.Error(), 160), fmtDuration(cooldown))
			continue
		}

		if p.cfg.retryable(resp.StatusCode) {
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			snippet := readErrorSnippet(resp.Body)
			_ = resp.Body.Close()

			detail := fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
			if snippet != "" {
				detail += ": " + snippet
			}
			cooldown := p.pool.Fail(pick.index, resp.StatusCode, retryAfter, detail)
			p.failovers.Add(1)
			lastFailure = attemptFailure{
				upstream: up.Name, status: resp.StatusCode, detail: snippet, cooldown: cooldown,
			}
			tail := "rotating"
			if attempt == attempts {
				tail = "no attempts left"
			}
			msg := fmt.Sprintf("%s -> %d %s (attempt %d/%d)", up.Name, resp.StatusCode,
				http.StatusText(resp.StatusCode), attempt, attempts)
			if snippet != "" {
				msg += ": " + snippet
			}
			p.log.failf(reqID, "%s; cooling %s, %s", msg, fmtDuration(cooldown), tail)
			continue
		}

		// Commit this response to the client. From here on the request can no
		// longer be retried because bytes may already be on the wire.
		if resp.StatusCode >= 500 {
			cooldown := p.pool.Fail(pick.index, resp.StatusCode, 0,
				fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
			p.log.debugf(reqID, "%s returned %d (not in retry_statuses); cooling %s but passing response through",
				up.Name, resp.StatusCode, fmtDuration(cooldown))
		} else {
			p.pool.Success(pick.index, resp.StatusCode, time.Since(reqStart))
		}
		p.commitResponse(w, r, resp, up, reqID, started, streamRequested)
		return
	}

	if r.Context().Err() != nil {
		p.log.debugf(reqID, "client disconnected; abandoning request")
		return
	}

	status := http.StatusBadGateway
	if lastFailure.status == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	}
	msg := fmt.Sprintf("all upstreams failed after %d attempt(s)", attempted)
	if lastFailure.upstream != "" {
		msg += fmt.Sprintf(" (last: %s", lastFailure.upstream)
		if lastFailure.status != 0 {
			msg += fmt.Sprintf(" -> %d %s", lastFailure.status, http.StatusText(lastFailure.status))
		} else if lastFailure.detail != "" {
			msg += ": " + lastFailure.detail
		}
		msg += ")"
	}
	if status == http.StatusTooManyRequests && lastFailure.cooldown > 0 {
		secs := int(math.Ceil(lastFailure.cooldown.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	p.log.errorf(reqID, "%s; returning %d", msg, status)
	writeAPIError(w, status, "upstream_exhausted", msg)
}

type attemptFailure struct {
	upstream string
	status   int
	detail   string
	cooldown time.Duration
}

func (p *Proxy) commitResponse(w http.ResponseWriter, r *http.Request, resp *http.Response,
	up *Upstream, reqID string, started time.Time, streamRequested bool) {
	defer resp.Body.Close()

	src := resp.Header.Clone()
	removeHopHeaders(src)
	src.Del("Content-Length")
	src.Set("X-Rotator-Upstream", up.Name)
	src.Set("X-Rotator-Request-Id", reqID)

	dst := w.Header()
	for k, values := range src {
		for _, v := range values {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush() // send headers immediately, before the first token
	}
	written, copyErr := copyAndFlush(w, resp.Body, flusher)
	elapsed := time.Since(started)

	if copyErr != nil && r.Context().Err() != nil {
		p.log.debugf(reqID, "%s client disconnected after %s (%s)", up.Name, fmtDuration(elapsed), fmtBytes(written))
		return
	}
	if copyErr != nil {
		p.log.warnf(reqID, "%s response interrupted after %s (%s): %v",
			up.Name, fmtDuration(elapsed), fmtBytes(written), copyErr)
	}

	sse := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	marker := ""
	if sse {
		marker = " sse"
	} else if streamRequested {
		marker = " stream"
	}
	line := fmt.Sprintf("%s %d | %s | %s%s", up.Name, resp.StatusCode, fmtBytes(written), fmtDuration(elapsed), marker)
	if resp.StatusCode >= 400 {
		p.log.replyf(reqID, "%s", line)
	} else {
		p.log.okf(reqID, "%s", line)
	}
}

var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32<<10)
		return &b
	},
}

func copyAndFlush(dst io.Writer, src io.Reader, flusher http.Flusher) (int64, error) {
	bp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bp)
	buf := *bp

	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			wn, writeErr := dst.Write(buf[:n])
			total += int64(wn)
			if writeErr != nil {
				return total, writeErr
			}
			if wn != n {
				return total, io.ErrShortWrite
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func (p *Proxy) forwardHeaders(in http.Header) http.Header {
	h := in.Clone()
	removeHopHeaders(h)
	h.Del("Accept-Encoding") // keep SSE unbuffered; pass identity bytes through
	h.Del("Content-Length")
	h.Del("Expect")

	// Never leak the proxy's own client token to an upstream.
	if p.cfg.ClientAPIKey != "" {
		if tokensEqual(bearerToken(h.Get("Authorization")), p.cfg.ClientAPIKey) {
			h.Del("Authorization")
		}
		if tokensEqual(strings.TrimSpace(h.Get("X-Api-Key")), p.cfg.ClientAPIKey) {
			h.Del("X-Api-Key")
		}
	}
	return h
}

func (u *Upstream) applyHeaders(h http.Header) {
	if u.APIKey != "" {
		// Drop any client-supplied credentials so a dummy/placeholder key can
		// never override or accompany the configured credential.
		h.Del("Authorization")
		h.Del("X-Api-Key")
		h.Set(u.AuthHeader, u.AuthPrefix+u.APIKey)
	}
	for k, values := range u.Headers {
		h.Del(k)
		for _, v := range values {
			h.Add(k, v)
		}
	}
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, connection := range h.Values("Connection") {
		for _, token := range strings.Split(connection, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func (p *Proxy) authorized(r *http.Request) bool {
	want := p.cfg.ClientAPIKey
	if want == "" {
		return true
	}
	if tokensEqual(bearerToken(r.Header.Get("Authorization")), want) {
		return true
	}
	return tokensEqual(strings.TrimSpace(r.Header.Get("X-Api-Key")), want)
}

func bearerToken(v string) string {
	const prefix = "bearer "
	if len(v) >= len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}

func tokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var errBodyTooLarge = errors.New("request body too large")

func readRequestBody(r *http.Request, max int64) ([]byte, error) {
	if r.ContentLength > max {
		return nil, errBodyTooLarge
	}
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func requestMeta(body []byte) (model string, stream bool) {
	if len(body) == 0 || len(body) > 1<<20 {
		return "", false
	}
	var m struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false
	}
	return m.Model, m.Stream
}

func readErrorSnippet(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	if len(bytes.TrimSpace(b)) == 0 {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &envelope); err == nil && envelope.Error.Message != "" {
		return sanitizeLog(envelope.Error.Message, 160)
	}
	return sanitizeLog(string(b), 160)
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(v); err == nil {
		if seconds <= 0 {
			return 0
		}
		d := time.Duration(seconds) * time.Second
		if d > 24*time.Hour {
			d = 24 * time.Hour
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > 24*time.Hour {
				d = 24 * time.Hour
			}
			return d
		}
	}
	return 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "proxy_rotator_error",
			"code":    code,
		},
	})
}

func newRequestID() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%010x", time.Now().UnixNano()&0xffffffffff)
}
