// Package logging provides structured logging that keeps sensitive data out of
// application logs.
//
// AGENTS.md requires that logs allow an operation to be investigated without
// revealing secret or personal content. The enforcement point is a
// slog.Handler wrapper: it inspects every attribute before it is written, so a
// careless call site cannot leak a passport number or an API key by forgetting
// to redact it.
package logging

import (
	"context"
	"log/slog"
	"strings"
)

// sensitiveKeys are attribute names whose values must never be written. The
// match is case-insensitive and substring-based, so "user_passport" and
// "PII_PASSPORT" are both caught.
var sensitiveKeys = []string{
	"api_key", "apikey", "authorization", "auth",
	"password", "passwd", "secret", "token", "credential",
	"passport", "dob", "date_of_birth", "birthdate",
	"full_name", "fullname", "first_name", "last_name", "surname",
	"email", "phone", "address",
	"dsn", "connection_string",
	"card", "pan", "cvv", "iban", "billing",
	"booking_token",
	"raw_query", "prompt", "messages", "transcript",
}

// Redacted is the placeholder written instead of a sensitive value.
const Redacted = "[redacted]"

// IsSensitiveKey reports whether an attribute name must be redacted.
func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(key)

	for _, needle := range sensitiveKeys {
		if strings.Contains(lower, needle) {
			return true
		}
	}

	return false
}

// RedactValue masks a value that is known to be sensitive even when its
// attribute name is innocuous, such as a raw response body.
func RedactValue(v string) string {
	if v == "" {
		return ""
	}

	return Redacted
}

// Handler wraps a slog.Handler and redacts sensitive attributes, including
// those inside groups.
type Handler struct {
	inner slog.Handler
}

// NewHandler wraps inner with redaction.
func NewHandler(inner slog.Handler) *Handler {
	return &Handler{inner: inner}
}

// Enabled delegates to the wrapped handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle redacts and delegates.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))

		return true
	})

	if ctx == nil {
		// A nil context would panic inside the wrapped handler.
		ctx = context.Background()
	}

	return h.inner.Handle(ctx, out)
}

// WithAttrs redacts the supplied attributes and delegates.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		redacted = append(redacted, redactAttr(a))
	}

	return &Handler{inner: h.inner.WithAttrs(redacted)}
}

// WithGroup delegates. A group name is not itself redacted; the attributes
// inside it are, when they are eventually handled.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}

	// Groups are resolved lazily by slog, so resolve the value first and
	// redact the leaves.
	value := a.Value.Resolve()

	if value.Kind() == slog.KindGroup {
		group := value.Group()
		out := make([]slog.Attr, 0, len(group))

		for _, child := range group {
			out = append(out, redactAttr(child))
		}

		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}

	return a
}
