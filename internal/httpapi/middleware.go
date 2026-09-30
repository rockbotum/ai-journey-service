package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/auth"
)

type ctxKey int

const requestIDKey ctxKey = iota

// requestIDHeader carries a caller-supplied correlation id. It is only trusted
// for correlation: it is never used for authorisation or as a lookup key.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLen bounds a caller-supplied id so it cannot be used to inject
// megabytes into the logs.
const maxRequestIDLen = 64

// newRequestID returns an unguessable identifier.
func newRequestID() string {
	var buf [16]byte

	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is fatal for correlation but must not fail the
		// request; a timestamp-derived id is still useful for a human.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}

	return hex.EncodeToString(buf[:])
}

// requestIDMiddleware attaches a request id to the context and the response.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}

		w.Header().Set(requestIDHeader, id)

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)

	return id
}

// sanitizeRequestID keeps only printable characters of a bounded length.
func sanitizeRequestID(raw string) string {
	if len(raw) == 0 || len(raw) > maxRequestIDLen {
		return ""
	}

	out := make([]byte, 0, len(raw))

	for i := range len(raw) {
		c := raw[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		}
	}

	if len(out) == 0 {
		return ""
	}

	return string(out)
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}

	n, err := s.ResponseWriter.Write(p)
	s.bytes += n

	return n, err
}

// loggingMiddleware writes one structured access log line per request. It logs
// identifiers, status and latency, never a body or a header value.
func loggingMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}

			log.InfoContext(r.Context(), "http request",
				"method", r.Method,
				// r.URL.Path is a route template, not raw user input, but it is
				// still trimmed to a sane length.
				"path", truncate(r.URL.Path, 128),
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", requestIDFrom(r.Context()),
			)
		})
	}
}

// recoverMiddleware turns a panic into a 500 without leaking the panic value
// or a stack trace to the client.
func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// The panic value is logged, never returned: it may contain
					// a supplier payload or personal data.
					log.ErrorContext(r.Context(), "handler panicked",
						"request_id", requestIDFrom(r.Context()),
						"panic", truncate(fmt.Sprintf("%v", rec), 200),
					)

					writeError(w, r, http.StatusInternalServerError, codeInternal, "An unexpected error occurred.")
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// maxBodyMiddleware caps the request body before any handler reads it.
func maxBodyMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeError(w, r, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "The request body is too large.")

				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, limit)

			next.ServeHTTP(w, r)
		})
	}
}

// rateLimiter is a fixed-window counter per client key.
//
// It is per-process. A multi-instance deployment needs a shared limiter, which
// is recorded as a known limitation in the ADR rather than hidden.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	perMin  time.Duration
	windows map[string]*window
	now     func() time.Time
}

type window struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, per time.Duration, now func() time.Time) *rateLimiter {
	return &rateLimiter{limit: limit, perMin: per, windows: make(map[string]*window), now: now}
}

// allow reports whether a request from key may proceed.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= l.perMin {
		l.windows[key] = &window{count: 1, start: now}

		return true
	}

	if w.count >= l.limit {
		return false
	}

	w.count++

	return true
}

// sweep drops windows that have rolled over, so a long-running process does
// not accumulate one entry per client forever.
func (l *rateLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	for key, w := range l.windows {
		if now.Sub(w.start) >= l.perMin {
			delete(l.windows, key)
		}
	}
}

// rateLimitMiddleware enforces the per-client budget.
func rateLimitMiddleware(limiter *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.allow(clientKey(r)) {
				w.Header().Set("Retry-After", "60")
				writeError(w, r, http.StatusTooManyRequests, codeRateLimited, "Too many requests.")

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientKey identifies the caller for rate limiting. The authenticated user is
// preferred; the peer address is only a fallback for unauthenticated routes.
func clientKey(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok && p.UserID != "" {
		return "user:" + p.UserID
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}

	return "ip:" + host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n]
}
