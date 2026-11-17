package main

import (
	"encoding/json"
	"math"
	"net/http"
	"time"
)

type adminStatus struct {
	Service        string           `json:"service"`
	Version        string           `json:"version"`
	UptimeSeconds  float64          `json:"uptime_seconds"`
	Listen         string           `json:"listen"`
	MaxAttempts    int              `json:"max_attempts"`
	ActiveRequests int64            `json:"active_requests"`
	TotalRequests  uint64           `json:"total_requests"`
	Failovers      uint64           `json:"failovers"`
	Upstreams      []upstreamStatus `json:"upstreams"`
}

func (p *Proxy) statusSnapshot() adminStatus {
	return adminStatus{
		Service:        "proxy-rotator",
		Version:        version,
		UptimeSeconds:  math.Round(time.Since(p.started).Seconds()*10) / 10,
		Listen:         p.cfg.Listen,
		MaxAttempts:    p.cfg.MaxAttempts,
		ActiveRequests: p.active.Load(),
		TotalRequests:  p.requests.Load(),
		Failovers:      p.failovers.Load(),
		Upstreams:      p.pool.Snapshot(time.Now()),
	}
}

// AdminHandler serves status and a manual cooldown reset. Bind it to loopback.
func (p *Proxy) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		switch r.URL.Path {
		case "/healthz":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				methodNotAllowed(w, "GET, HEAD")
				return
			}
			writeAdminJSON(w, http.StatusOK, map[string]any{
				"status": "ok", "service": "proxy-rotator", "version": version,
			})
		case "/status":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				methodNotAllowed(w, "GET, HEAD")
				return
			}
			writeAdminJSON(w, http.StatusOK, p.statusSnapshot())
		case "/reset":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			n := p.pool.ResetCooldowns()
			p.log.warnf("", "admin: reset cooldowns for %d upstream(s)", n)
			writeAdminJSON(w, http.StatusOK, map[string]any{
				"reset": true, "upstreams": n,
			})
		default:
			writeAdminJSON(w, http.StatusNotFound, map[string]any{
				"error": "not found",
			})
		}
	})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeAdminJSON(w, http.StatusMethodNotAllowed, map[string]any{
		"error": "method not allowed",
	})
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
