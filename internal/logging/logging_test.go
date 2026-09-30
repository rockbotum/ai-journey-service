package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/rockbotum/ai-journey-service/internal/logging"
)

func newLogger(buf *bytes.Buffer) *slog.Logger {
	handler := logging.NewHandler(slog.NewJSONHandler(buf, nil))

	return slog.New(handler)
}

func TestSensitiveAttributesAreRedacted(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"api key", "api_key", "sk-live-secret"},
		{"authorization header", "authorization", "Bearer abc123"},
		{"password", "user_password", "hunter2"},
		{"passport", "passport_number", "1234567890"},
		{"date of birth", "dob", "1990-01-01"},
		{"passenger name", "passenger_full_name", "A. Passenger"},
		{"email", "email", "a@example.com"},
		{"database dsn", "db_dsn", "postgres://u:p@host/db"},
		{"payment card", "card_number", "4111111111111111"},
		{"booking token", "booking_token", "tok_secret"},
		{"prompt body", "system_prompt", "you are a travel assistant"},
		{"raw user query", "raw_query", "flight to new york"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			newLogger(&buf).Info("test", tc.key, tc.value)

			out := buf.String()
			if strings.Contains(out, tc.value) {
				t.Errorf("value %q leaked into the log: %s", tc.value, out)
			}

			if !strings.Contains(out, logging.Redacted) {
				t.Errorf("expected a redaction marker in: %s", out)
			}
		})
	}
}

func TestNonSensitiveAttributesArePreserved(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf).Info("booking confirmed",
		"booking_id", "bk_123",
		"state", "confirmed",
		"supplier_id", "sup_a",
		"amount_minor", 45000,
	)

	out := buf.String()
	for _, want := range []string{"bk_123", "confirmed", "sup_a", "45000"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q to survive redaction: %s", want, out)
		}
	}
}

func TestNestedGroupAttributesAreRedacted(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf).Info("user", slog.Group("user",
		slog.String("id", "user_1"),
		slog.String("passport_number", "1234567890"),
	))

	out := buf.String()
	if !strings.Contains(out, "user_1") {
		t.Errorf("non-sensitive group member should survive: %s", out)
	}

	if strings.Contains(out, "1234567890") {
		t.Errorf("nested sensitive value leaked: %s", out)
	}
}

func TestWithAttrsRedactsAtBinding(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf).With("api_key", "sk-live-secret")

	logger.Info("no attributes here")

	if strings.Contains(buf.String(), "sk-live-secret") {
		t.Errorf("WithAttrs leaked a secret: %s", buf.String())
	}
}

func TestErrorValueIsNotRedactedByKeyAlone(t *testing.T) {
	// An error may carry a provider message. The key "err" is not sensitive by
	// name, so redaction is the caller's job; this test documents that the
	// handler does not silently mangle technical context that investigations
	// depend on.
	var buf bytes.Buffer
	newLogger(&buf).Error("request failed", "err", "connection refused")

	if !strings.Contains(buf.String(), "connection refused") {
		t.Errorf("technical error context must be preserved: %s", buf.String())
	}
}

func TestHandleToleratesNilContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf)

	//nolint:staticcheck // deliberately passing a nil context to prove the handler is defensive.
	logger.InfoContext(nil, "no context") //nolint:staticcheck

	if !strings.Contains(buf.String(), "no context") {
		t.Errorf("expected the message to be written: %s", buf.String())
	}
}

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{"API_KEY", "user_password", "BookingToken", "db_dsn", "passport_number"}
	for _, k := range sensitive {
		if !logging.IsSensitiveKey(k) {
			t.Errorf("%q should be sensitive", k)
		}
	}

	safe := []string{"booking_id", "state", "amount_minor", "supplier_id", "duration_ms"}
	for _, k := range safe {
		if logging.IsSensitiveKey(k) {
			t.Errorf("%q should not be sensitive", k)
		}
	}
}
