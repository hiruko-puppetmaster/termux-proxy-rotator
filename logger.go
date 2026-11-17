package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// LogLevel controls how chatty the logger is.
type LogLevel int

const (
	LevelQuiet LogLevel = iota // startup, failovers and errors only
	LevelInfo                  // + one line per request
	LevelDebug                 // + pool/selection details
)

func parseLogLevel(s string) (LogLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return LevelInfo, nil
	case "quiet", "warn", "failures":
		return LevelQuiet, nil
	case "debug":
		return LevelDebug, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log_level %q (want quiet, info or debug)", s)
	}
}

const (
	ansiReset   = "\x1b[0m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
)

// Logger writes compact, timestamped lines. It is safe for concurrent use.
type Logger struct {
	mu       sync.Mutex
	w        io.Writer
	minLevel LogLevel
	color    bool
}

func NewLogger(w io.Writer, level LogLevel) *Logger {
	return &Logger{w: w, minLevel: level, color: supportsColor(w)}
}

func supportsColor(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (l *Logger) write(minLevel LogLevel, color, label, reqID, format string, args ...any) {
	if l == nil || l.minLevel < minLevel {
		return
	}
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("15:04:05.000")

	var b strings.Builder
	b.Grow(len(msg) + 64)
	if l.color {
		b.WriteString(ansiDim)
		b.WriteString(ts)
		b.WriteString(ansiReset)
		b.WriteString("  ")
		b.WriteString(color)
		b.WriteString(fmt.Sprintf("%-6s", label))
		b.WriteString(ansiReset)
	} else {
		b.WriteString(ts)
		b.WriteString("  ")
		b.WriteString(fmt.Sprintf("%-6s", label))
	}
	if reqID != "" {
		b.WriteString("  ")
		if l.color {
			b.WriteString(ansiDim)
		}
		b.WriteString("[")
		b.WriteString(reqID)
		b.WriteString("]")
		if l.color {
			b.WriteString(ansiReset)
		}
	}
	b.WriteString("  ")
	b.WriteString(msg)
	b.WriteByte('\n')

	l.mu.Lock()
	_, _ = io.WriteString(l.w, b.String())
	l.mu.Unlock()
}

// Request, success and reply lines are hidden in quiet mode. Everything else
// (failovers, warnings, errors, startup) is always printed.

func (l *Logger) requestf(reqID, format string, args ...any) {
	l.write(LevelInfo, ansiCyan, "REQ", reqID, format, args...)
}

func (l *Logger) okf(reqID, format string, args ...any) {
	l.write(LevelInfo, ansiGreen, "OK", reqID, format, args...)
}

func (l *Logger) replyf(reqID, format string, args ...any) {
	l.write(LevelInfo, ansiYellow, "REPLY", reqID, format, args...)
}

func (l *Logger) failf(reqID, format string, args ...any) {
	l.write(LevelQuiet, ansiYellow, "FAIL", reqID, format, args...)
}

func (l *Logger) warnf(reqID, format string, args ...any) {
	l.write(LevelQuiet, ansiYellow, "WARN", reqID, format, args...)
}

func (l *Logger) errorf(reqID, format string, args ...any) {
	l.write(LevelQuiet, ansiRed, "ERROR", reqID, format, args...)
}

func (l *Logger) infof(format string, args ...any) {
	l.write(LevelQuiet, ansiMagenta, "INFO", "", format, args...)
}

func (l *Logger) debugf(reqID, format string, args ...any) {
	l.write(LevelDebug, ansiDim, "DEBUG", reqID, format, args...)
}

func fmtDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%dus", d.Microseconds())
	}
}

func fmtBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n) / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// sanitizeLog makes untrusted strings safe for one-line terminal output.
func sanitizeLog(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return -1 // includes ESC; prevents terminal escape injection
		default:
			return r
		}
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > maxRunes {
		s = string(r[:maxRunes]) + "..."
	}
	return s
}
