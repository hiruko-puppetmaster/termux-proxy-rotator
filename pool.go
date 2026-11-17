package main

import (
	"math"
	"sync"
	"time"
)

// Pool owns upstream health state. All selection and result recording goes
// through one mutex; contention is negligible compared with network I/O.
type Pool struct {
	mu        sync.Mutex
	cfg       *Settings
	upstreams []*Upstream
	rr        uint64
	now       func() time.Time
}

func NewPool(cfg *Settings) *Pool {
	return &Pool{cfg: cfg, upstreams: cfg.Upstreams, now: time.Now}
}

type pickResult struct {
	index  int
	forced bool
	wait   time.Duration
}

// Pick chooses the best untried upstream: the lowest priority number with any
// healthy member, round-robin inside that priority tier. If everything is
// cooling down and allowForced is set it returns the endpoint that recovers
// soonest and marks the pick as forced.
func (p *Pool) Pick(exclude map[int]struct{}, allowForced bool) (pickResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	bestPriority := 0
	foundHealthy := false
	for i, u := range p.upstreams {
		if _, skip := exclude[i]; skip || !u.Enabled {
			continue
		}
		if !u.cooldownUntil.After(now) && (!foundHealthy || u.Priority < bestPriority) {
			bestPriority = u.Priority
			foundHealthy = true
		}
	}

	if foundHealthy {
		tier := make([]int, 0, len(p.upstreams))
		for i, u := range p.upstreams {
			if _, skip := exclude[i]; skip || !u.Enabled {
				continue
			}
			if u.Priority == bestPriority && !u.cooldownUntil.After(now) {
				tier = append(tier, i)
			}
		}
		if len(tier) > 0 {
			idx := tier[int(p.rr%uint64(len(tier)))]
			p.rr++
			p.upstreams[idx].attempts++
			return pickResult{index: idx}, true
		}
	}

	// All remaining candidates are cooling down. Try the one that is closest
	// to recovery, but the caller limits this to one forced attempt.
	if !allowForced {
		return pickResult{}, false
	}
	best := -1
	var bestUntil time.Time
	for i, u := range p.upstreams {
		if _, skip := exclude[i]; skip || !u.Enabled {
			continue
		}
		if best == -1 || u.cooldownUntil.Before(bestUntil) {
			best = i
			bestUntil = u.cooldownUntil
		}
	}
	if best == -1 {
		return pickResult{}, false
	}
	p.upstreams[best].attempts++
	return pickResult{index: best, forced: true, wait: bestUntil.Sub(now)}, true
}

// Fail cools an upstream down with exponential backoff. A server-provided
// Retry-After is honored directly (still capped by max_cooldown_seconds).
// status == 0 means a transport/network error.
func (p *Pool) Fail(index, status int, retryAfter time.Duration, detail string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	u := p.upstreams[index]
	now := p.now()
	u.consecutiveFailures++
	u.failures++
	u.lastStatus = status
	u.lastUsed = now
	u.lastError = sanitizeLog(detail, 240)

	cooldown := p.cooldownFor(status)
	if retryAfter > 0 {
		cooldown = retryAfter
	} else {
		shift := u.consecutiveFailures - 1
		if shift > 6 {
			shift = 6
		}
		mult := time.Duration(1) << shift
		if cooldown > p.cfg.MaxCooldown/mult {
			cooldown = p.cfg.MaxCooldown
		} else {
			cooldown *= mult
		}
	}
	if cooldown > p.cfg.MaxCooldown {
		cooldown = p.cfg.MaxCooldown
	}
	if cooldown < time.Second {
		cooldown = time.Second
	}
	u.cooldownUntil = now.Add(cooldown)
	return cooldown
}

func (p *Pool) cooldownFor(status int) time.Duration {
	if status == 0 {
		return p.cfg.NetworkCooldown
	}
	if d, ok := p.cfg.Cooldowns[status]; ok {
		return d
	}
	return p.cfg.DefaultCooldown
}

// Success records a delivered upstream response (including non-retryable 4xx)
// and clears any previous cooldown.
func (p *Pool) Success(index, status int, latency time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	u := p.upstreams[index]
	u.successes++
	u.consecutiveFailures = 0
	u.cooldownUntil = time.Time{}
	u.lastStatus = status
	u.lastUsed = p.now()
	u.lastLatency = latency
	u.lastError = ""
}

// ResetCooldowns gives every upstream a clean slate without clearing counters.
func (p *Pool) ResetCooldowns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range p.upstreams {
		u.consecutiveFailures = 0
		u.cooldownUntil = time.Time{}
		u.lastError = ""
	}
	return len(p.upstreams)
}

type upstreamStatus struct {
	Name                     string  `json:"name"`
	BaseURL                  string  `json:"base_url"`
	Priority                 int     `json:"priority"`
	Healthy                  bool    `json:"healthy"`
	CooldownRemainingSeconds float64 `json:"cooldown_remaining_seconds"`
	ConsecutiveFailures      int     `json:"consecutive_failures"`
	Attempts                 int64   `json:"attempts"`
	Successes                int64   `json:"successes"`
	Failures                 int64   `json:"failures"`
	LastStatus               int     `json:"last_status,omitempty"`
	LastError                string  `json:"last_error,omitempty"`
	LastUsed                 string  `json:"last_used,omitempty"`
	LastLatencyMs            int64   `json:"last_latency_ms,omitempty"`
}

func (p *Pool) Snapshot(now time.Time) []upstreamStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]upstreamStatus, 0, len(p.upstreams))
	for _, u := range p.upstreams {
		remaining := 0.0
		healthy := !u.cooldownUntil.After(now)
		if !healthy {
			remaining = math.Round(u.cooldownUntil.Sub(now).Seconds()*10) / 10
		}
		s := upstreamStatus{
			Name:                     u.Name,
			BaseURL:                  u.displayURL(),
			Priority:                 u.Priority,
			Healthy:                  healthy,
			CooldownRemainingSeconds: remaining,
			ConsecutiveFailures:      u.consecutiveFailures,
			Attempts:                 u.attempts,
			Successes:                u.successes,
			Failures:                 u.failures,
			LastError:                u.lastError,
		}
		if u.lastStatus != 0 {
			s.LastStatus = u.lastStatus
		}
		if !u.lastUsed.IsZero() {
			s.LastUsed = u.lastUsed.Format(time.RFC3339)
		}
		if u.lastLatency > 0 {
			s.LastLatencyMs = u.lastLatency.Milliseconds()
		}
		out = append(out, s)
	}
	return out
}
