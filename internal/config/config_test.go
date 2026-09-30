package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// setEnv sets a variable for the duration of a test and restores it after, so
// tests stay independent of execution order.
func setEnv(t *testing.T, key, value string) {
	t.Helper()

	prev, had := lookupEnv(key)
	if err := setEnvRaw(key, value); err != nil {
		t.Fatalf("setenv %s: %v", key, err)
	}

	t.Cleanup(func() {
		if had {
			_ = setEnvRaw(key, prev)

			return
		}

		_ = unsetEnvRaw(key)
	})
}

// setSupplierEnv seeds the supplier settings that are always required, so a
// test can focus on the variable it actually exercises.
func setSupplierEnv(t *testing.T) {
	t.Helper()

	setEnv(t, "SUPPLIER_BASE_URL", "https://supplier.example.com")
	setEnv(t, "SUPPLIER_ID", "sup-1")
	setEnv(t, "SUPPLIER_API_KEY", "supplier-secret-key")
}

func clearEnv(t *testing.T, keys ...string) {
	t.Helper()

	for _, k := range keys {
		setEnv(t, k, "")
	}
}

func TestLoadRequiresDatabaseDSN(t *testing.T) {
	clearEnv(t, "DB_DSN", "AI_API_KEY", "SUPPLIER_API_KEY", "SUPPLIER_BASE_URL", "SUPPLIER_ID")
	setSupplierEnv(t)

	_, err := Load()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("err = %v, want ErrMissingRequired", err)
	}
}

func TestLoadAppliesSafeDefaults(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AUTH_CREDENTIALS", "user_1:secret-key-1")
	setSupplierEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.AI.Enabled {
		t.Fatal("AI must be disabled by default")
	}

	if cfg.HTTP.Listen != ":8080" {
		t.Fatalf("listen = %q, want :8080", cfg.HTTP.Listen)
	}

	if cfg.HTTP.MaxBodyBytes != 64*1024 {
		t.Fatalf("max body = %d, want 65536", cfg.HTTP.MaxBodyBytes)
	}
}

func TestEnablingAIWithoutCredentialsFailsAtStartup(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AI_ENABLED", "true")
	clearEnv(t, "AI_BASE_URL", "AI_MODEL", "AI_API_KEY", "AI_SYSTEM_PROMPT_FILE")

	_, err := Load()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("err = %v, want ErrMissingRequired for AI settings", err)
	}

	msg := err.Error()
	for _, want := range []string{"AI_BASE_URL", "AI_MODEL", "AI_API_KEY", "AI_SYSTEM_PROMPT_FILE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s: %s", want, msg)
		}
	}
}

func TestInvalidValuesAreReported(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "HTTP_READ_TIMEOUT", "not-a-duration")
	setEnv(t, "HTTP_MAX_BODY_BYTES", "0")

	_, err := Load()
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
}

func TestAllErrorsAreReportedAtOnce(t *testing.T) {
	clearEnv(t, "DB_DSN", "AI_API_KEY")
	setEnv(t, "HTTP_READ_TIMEOUT", "bad")
	setEnv(t, "AI_MAX_RETRIES", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected errors")
	}

	msg := err.Error()
	for _, want := range []string{"DB_DSN", "HTTP_READ_TIMEOUT", "AI_MAX_RETRIES"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s: %s", want, msg)
		}
	}
}

func TestIdleConnsCannotExceedOpenConns(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "DB_MAX_OPEN_CONNS", "5")
	setEnv(t, "DB_MAX_IDLE_CONNS", "10")

	_, err := Load()
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
}

// TestSecretsAreRedacted is the guard required by AGENTS.md: a secret must not
// be printable by accident, and must stay usable at the point of use.
func TestSecretsAreRedacted(t *testing.T) {
	secretValue := "sk-live-do-not-log-me"
	setEnv(t, "DB_DSN", "postgres://user:secretpass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AI_ENABLED", "true")
	setEnv(t, "AI_BASE_URL", "https://ai.example.com")
	setEnv(t, "AI_MODEL", "model-x")
	setEnv(t, "AI_API_KEY", secretValue)
	setEnv(t, "AI_SYSTEM_PROMPT_FILE", "configs/ai_system.md")
	setEnv(t, "AUTH_CREDENTIALS", "user_1:secret-key-1")
	setSupplierEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.AI.APIKey.Reveal() != secretValue {
		t.Fatal("Reveal must return the real value at the point of use")
	}

	if cfg.DB.DSN.IsZero() {
		t.Fatal("DSN must be readable when it is configured")
	}

	// The secret value must never render, through any formatting verb.
	rendered := []string{
		fmt.Sprintf("%v", cfg.AI.APIKey),
		fmt.Sprintf("%s", cfg.AI.APIKey),
		fmt.Sprintf("%q", cfg.DB.DSN),
		fmt.Sprintf("%v", cfg),
	}

	for _, r := range rendered {
		if strings.Contains(r, secretValue) {
			t.Errorf("secret value leaked into rendered output %q", r)
		}
	}
}

// TestErrorsNeverLeakSecretValues covers the failure path separately, because a
// failed Load returns a zero Config and would otherwise mask the assertion.
func TestErrorsNeverLeakSecretValues(t *testing.T) {
	secretValue := "sk-live-do-not-log-me"
	setEnv(t, "DB_DSN", "postgres://user:secretpass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AI_ENABLED", "true")
	setEnv(t, "AI_BASE_URL", "https://ai.example.com")
	setEnv(t, "AI_MODEL", "model-x")
	setEnv(t, "AI_API_KEY", secretValue)
	setEnv(t, "AI_SYSTEM_PROMPT_FILE", "configs/ai_system.md")
	setEnv(t, "DB_MAX_OPEN_CONNS", "1")
	setEnv(t, "DB_MAX_IDLE_CONNS", "999") // exceeds open conns, forcing an error

	_, err := Load()
	if err == nil {
		t.Fatal("expected a validation error")
	}

	if strings.Contains(err.Error(), secretValue) {
		t.Error("configuration error leaked the AI API key value")
	}

	if strings.Contains(err.Error(), "secretpass") {
		t.Error("configuration error leaked the database password")
	}
}

func TestLoadRequiresCredentials(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AUTH_CREDENTIALS", "")
	setSupplierEnv(t)

	_, err := Load()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("err = %v, want ErrMissingRequired", err)
	}
}

func TestParseCredentials(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{name: "single", raw: "user_1:key-1", want: 1},
		{name: "several", raw: "user_1:key-1;user_2:key-2", want: 2},
		{name: "with scopes", raw: "user_1:key-1:bookings:write,bookings:read", want: 1},
		{name: "empty", raw: "", wantErr: true},
		{name: "whitespace only", raw: "   ", wantErr: true},
		{name: "no key", raw: "user_1", wantErr: true},
		{name: "empty key", raw: "user_1:", wantErr: true},
		{name: "empty user", raw: ":key-1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCredentials(tc.raw)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q", tc.raw)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseCredentials(%q): %v", tc.raw, err)
			}

			if len(got) != tc.want {
				t.Fatalf("got %d credentials, want %d", len(got), tc.want)
			}
		})
	}
}

// A malformed entry must be reported without echoing the secret it contained.
func TestParseCredentialsDoesNotEchoSecrets(t *testing.T) {
	_, err := parseCredentials("user_1:key-1;oops")
	if err == nil {
		t.Fatal("want an error for a malformed entry")
	}

	if strings.Contains(err.Error(), "key-1") {
		t.Fatalf("the error leaked the key: %v", err)
	}
}

func TestParsedCredentialKeepsScopesAndHidesKey(t *testing.T) {
	creds, err := parseCredentials("user_1:key-1:bookings:write,bookings:read")
	if err != nil {
		t.Fatalf("parseCredentials: %v", err)
	}

	if creds[0].UserID != "user_1" {
		t.Fatalf("UserID = %q, want user_1", creds[0].UserID)
	}

	if creds[0].Key.Reveal() != "key-1" {
		t.Fatalf("Reveal = %q, want key-1", creds[0].Key.Reveal())
	}

	if creds[0].Key.String() != "[redacted]" {
		t.Fatalf("String = %q, want [redacted]", creds[0].Key.String())
	}

	if len(creds[0].Scopes) != 2 {
		t.Fatalf("Scopes = %v, want two entries", creds[0].Scopes)
	}
}

func TestLoadRequiresSupplierSettings(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AUTH_CREDENTIALS", "user_1:secret-key-1")
	clearEnv(t, "SUPPLIER_BASE_URL", "SUPPLIER_ID", "SUPPLIER_API_KEY")

	_, err := Load()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("err = %v, want ErrMissingRequired", err)
	}

	// All three problems are reported at once, so a misconfigured deployment
	// does not need three restarts to discover three problems.
	msg := err.Error()
	for _, want := range []string{"SUPPLIER_BASE_URL", "SUPPLIER_ID", "SUPPLIER_API_KEY"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error does not mention %s: %v", want, err)
		}
	}
}

// A missing supplier key must be reported by name without exposing any other
// configured value.
func TestLoadDoesNotEchoOtherSecrets(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:supersecret@localhost:5432/db?sslmode=disable")
	setEnv(t, "AUTH_CREDENTIALS", "user_1:auth-secret")
	clearEnv(t, "SUPPLIER_API_KEY")
	setEnv(t, "SUPPLIER_BASE_URL", "https://supplier.example.com")
	setEnv(t, "SUPPLIER_ID", "sup-1")

	_, err := Load()
	if err == nil {
		t.Fatal("want an error for a missing supplier key")
	}

	if strings.Contains(err.Error(), "supersecret") || strings.Contains(err.Error(), "auth-secret") {
		t.Fatalf("the error leaked a secret: %v", err)
	}
}

func TestSupplierDefaultsAreSafe(t *testing.T) {
	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AUTH_CREDENTIALS", "user_1:secret-key-1")
	setSupplierEnv(t)
	clearEnv(t, "SUPPLIER_TIMEOUT", "SUPPLIER_MAX_RETRIES", "SUPPLIER_INITIAL_BACKOFF", "SUPPLIER_MAX_BACKOFF")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A zero timeout is the one configuration that must never be allowed, so a
	// default is mandatory.
	if cfg.Supplier.Timeout <= 0 {
		t.Fatalf("Timeout = %v, want a positive default", cfg.Supplier.Timeout)
	}

	if cfg.Supplier.MaxBackoff < cfg.Supplier.InitialBackoff {
		t.Fatalf("MaxBackoff %v is below InitialBackoff %v", cfg.Supplier.MaxBackoff, cfg.Supplier.InitialBackoff)
	}

	if cfg.Supplier.APIKey.IsZero() {
		t.Fatal("the supplier key must be readable when configured")
	}

	if cfg.Supplier.APIKey.String() != "[redacted]" {
		t.Fatalf("the supplier key must be redacted, got %q", cfg.Supplier.APIKey.String())
	}
}
