// Package config loads service configuration from the environment.
//
// Rules encoded here:
//   - secrets are read from the environment and never logged, printed or
//     included in any error returned to a caller;
//   - a missing optional value falls back to a documented safe default;
//   - a missing or invalid required value fails startup loudly rather than
//     silently degrading into a half-configured service.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Errors returned by Load. They are joined so a misconfigured deployment
// reports every problem at once instead of one per restart.
var (
	ErrMissingRequired = errors.New("config: required value is missing")
	ErrInvalidValue    = errors.New("config: invalid value")
)

// Secret is a string that refuses to be printed. Its String and GoString
// methods are redacted so a secret cannot reach a log or a %v verb by accident,
// and it has no MarshalJSON, which makes accidental serialisation a compile
// error rather than a data leak.
type Secret struct {
	value string
}

// NewSecret wraps a raw secret value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the underlying value. It is only called at the point of use,
// when configuring an outbound client.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret is unset.
func (s Secret) IsZero() bool { return s.value == "" }

func (Secret) String() string   { return "[redacted]" }
func (Secret) GoString() string { return "[redacted]" }

// Config is the fully validated service configuration.
type Config struct {
	Env      string
	HTTP     HTTPConfig
	AI       AIConfig
	DB       DBConfig
	Auth     AuthConfig
	Supplier SupplierConfig
}

// SupplierConfig configures the external travel supplier.
//
// The supplier is required, not optional: a booking service that starts without
// it would accept a user's intent and then fail at the moment it matters. AGENTS.md
// asks for a loud startup failure rather than a silent degradation.
type SupplierConfig struct {
	// BaseURL is the supplier API root.
	BaseURL string
	// APIKey authenticates the outbound call and is never logged.
	APIKey Secret
	// SupplierID is stored with every result so a booking can always be traced
	// back to its source.
	SupplierID     string
	Timeout        time.Duration
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// AuthConfig configures access to the public API.
//
// The service is not anonymous: every request must carry a credential that
// grants a user identity, because a booking belongs to a user and must not be
// readable by anyone else.
type AuthConfig struct {
	// Credentials maps a user id to the keys that act as that user. The
	// plaintext lives only in memory long enough to be hashed at startup.
	Credentials []Credential
}

// Credential is one API key together with the identity it grants.
type Credential struct {
	UserID string
	Key    Secret
	Scopes []string
}

// HTTPConfig configures the public API listener.
type HTTPConfig struct {
	Listen            string
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxBodyBytes      int64
	RequestsPerMinute int
}

// AIConfig configures the intent extraction boundary.
type AIConfig struct {
	Enabled         bool
	BaseURL         string
	Model           string
	APIKey          Secret
	Timeout         time.Duration
	MaxRetries      int
	MaxOutputTokens int
	// SystemPrompt is loaded from a file rather than embedded so it is
	// reviewable, but it is still configuration, not code.
	SystemPromptFile string
}

// DBConfig configures PostgreSQL access.
type DBConfig struct {
	DSN              Secret
	MaxOpenConns     int
	MaxIdleConns     int
	ConnMaxLifetime  time.Duration
	StatementTimeout time.Duration
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		Env:  get("APP_ENV", "development"),
		HTTP: HTTPConfig{},
		AI:   AIConfig{},
		DB:   DBConfig{},
	}

	// Access control. At least one credential is required: a service that
	// starts with no way to authenticate anybody is a misconfiguration, not a
	// convenient default.
	creds, credErr := parseCredentials(os.Getenv("AUTH_CREDENTIALS"))
	if credErr != nil {
		errs = append(errs, credErr)
	}

	cfg.Auth.Credentials = creds

	// Listener.
	cfg.HTTP.Listen = get("HTTP_LISTEN", ":8080")

	var err error
	if cfg.HTTP.ReadTimeout, err = duration("HTTP_READ_TIMEOUT", 10*time.Second); err != nil {
		errs = append(errs, err)
	}

	if cfg.HTTP.WriteTimeout, err = duration("HTTP_WRITE_TIMEOUT", 20*time.Second); err != nil {
		errs = append(errs, err)
	}

	if cfg.HTTP.IdleTimeout, err = duration("HTTP_IDLE_TIMEOUT", 60*time.Second); err != nil {
		errs = append(errs, err)
	}

	if cfg.HTTP.ShutdownTimeout, err = duration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		errs = append(errs, err)
	}

	if cfg.HTTP.MaxBodyBytes, err = int64Value("HTTP_MAX_BODY_BYTES", 64*1024); err != nil {
		errs = append(errs, err)
	}

	if cfg.HTTP.RequestsPerMinute, err = intValue("HTTP_REQUESTS_PER_MINUTE", 60); err != nil {
		errs = append(errs, err)
	}

	// External supplier.
	cfg.Supplier.BaseURL = get("SUPPLIER_BASE_URL", "")
	cfg.Supplier.SupplierID = get("SUPPLIER_ID", "")
	cfg.Supplier.APIKey = Secret{value: os.Getenv("SUPPLIER_API_KEY")}

	if cfg.Supplier.BaseURL == "" {
		errs = append(errs, fmt.Errorf("%w: SUPPLIER_BASE_URL", ErrMissingRequired))
	}

	if cfg.Supplier.SupplierID == "" {
		errs = append(errs, fmt.Errorf("%w: SUPPLIER_ID", ErrMissingRequired))
	}

	if cfg.Supplier.APIKey.IsZero() {
		errs = append(errs, fmt.Errorf("%w: SUPPLIER_API_KEY", ErrMissingRequired))
	}

	if cfg.Supplier.Timeout, err = duration("SUPPLIER_TIMEOUT", 8*time.Second); err != nil {
		errs = append(errs, err)
	}

	if cfg.Supplier.MaxRetries, err = intValue("SUPPLIER_MAX_RETRIES", 2); err != nil {
		errs = append(errs, err)
	}

	if cfg.Supplier.InitialBackoff, err = duration("SUPPLIER_INITIAL_BACKOFF", 200*time.Millisecond); err != nil {
		errs = append(errs, err)
	}

	if cfg.Supplier.MaxBackoff, err = duration("SUPPLIER_MAX_BACKOFF", 3*time.Second); err != nil {
		errs = append(errs, err)
	}

	// AI boundary.
	if cfg.AI.Enabled, err = boolValue("AI_ENABLED", false); err != nil {
		errs = append(errs, err)
	}

	cfg.AI.BaseURL = get("AI_BASE_URL", "")
	cfg.AI.Model = get("AI_MODEL", "")
	cfg.AI.APIKey = Secret{value: os.Getenv("AI_API_KEY")}
	cfg.AI.SystemPromptFile = get("AI_SYSTEM_PROMPT_FILE", "")

	if cfg.AI.Timeout, err = duration("AI_TIMEOUT", 15*time.Second); err != nil {
		errs = append(errs, err)
	}

	if cfg.AI.MaxRetries, err = intValue("AI_MAX_RETRIES", 2); err != nil {
		errs = append(errs, err)
	}

	if cfg.AI.MaxOutputTokens, err = intValue("AI_MAX_OUTPUT_TOKENS", 1024); err != nil {
		errs = append(errs, err)
	}

	// AI is optional, but enabling it without credentials must fail at startup
	// instead of at the first user request.
	if cfg.AI.Enabled {
		errs = append(errs, requireWhenEnabled("AI_BASE_URL", cfg.AI.BaseURL)...)
		errs = append(errs, requireWhenEnabled("AI_MODEL", cfg.AI.Model)...)
		errs = append(errs, requireWhenEnabled("AI_SYSTEM_PROMPT_FILE", cfg.AI.SystemPromptFile)...)

		if cfg.AI.APIKey.IsZero() {
			errs = append(errs, fmt.Errorf("%w: AI_API_KEY is required when AI_ENABLED=true", ErrMissingRequired))
		}
	}

	// Database.
	cfg.DB.DSN = Secret{value: os.Getenv("DB_DSN")}

	if cfg.DB.MaxOpenConns, err = intValue("DB_MAX_OPEN_CONNS", 10); err != nil {
		errs = append(errs, err)
	}

	if cfg.DB.MaxIdleConns, err = intValue("DB_MAX_IDLE_CONNS", 5); err != nil {
		errs = append(errs, err)
	}

	if cfg.DB.ConnMaxLifetime, err = duration("DB_CONN_MAX_LIFETIME", 30*time.Minute); err != nil {
		errs = append(errs, err)
	}

	if cfg.DB.StatementTimeout, err = duration("DB_STATEMENT_TIMEOUT", 5*time.Second); err != nil {
		errs = append(errs, err)
	}

	if cfg.DB.DSN.IsZero() {
		errs = append(errs, fmt.Errorf("%w: DB_DSN", ErrMissingRequired))
	}

	errs = append(errs, cfg.validate()...)

	if len(errs) > 0 {
		// errors.Join keeps every message but the values themselves are
		// environment variable names, never their contents.
		return Config{}, errors.Join(errs...)
	}

	return cfg, nil
}

func (c Config) validate() []error {
	var errs []error

	if c.HTTP.MaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("%w: HTTP_MAX_BODY_BYTES must be positive", ErrInvalidValue))
	}

	if c.HTTP.RequestsPerMinute <= 0 {
		errs = append(errs, fmt.Errorf("%w: HTTP_REQUESTS_PER_MINUTE must be positive", ErrInvalidValue))
	}

	if c.HTTP.ReadTimeout <= 0 || c.HTTP.WriteTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%w: HTTP timeouts must be positive", ErrInvalidValue))
	}

	if c.AI.MaxRetries < 0 {
		errs = append(errs, fmt.Errorf("%w: AI_MAX_RETRIES must not be negative", ErrInvalidValue))
	}

	if c.AI.MaxOutputTokens <= 0 {
		errs = append(errs, fmt.Errorf("%w: AI_MAX_OUTPUT_TOKENS must be positive", ErrInvalidValue))
	}

	if c.DB.MaxOpenConns <= 0 {
		errs = append(errs, fmt.Errorf("%w: DB_MAX_OPEN_CONNS must be positive", ErrInvalidValue))
	}

	if c.DB.MaxIdleConns > c.DB.MaxOpenConns {
		errs = append(errs, fmt.Errorf("%w: DB_MAX_IDLE_CONNS must not exceed DB_MAX_OPEN_CONNS", ErrInvalidValue))
	}

	return errs
}

func get(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return fallback
}

func requireWhenEnabled(key, value string) []error {
	if strings.TrimSpace(value) == "" {
		return []error{fmt.Errorf("%w: %s is required when AI_ENABLED=true", ErrMissingRequired, key)}
	}

	return nil
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration such as 15s", ErrInvalidValue, key)
	}

	if d <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive", ErrInvalidValue, key)
	}

	return d, nil
}

func intValue(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an integer", ErrInvalidValue, key)
	}

	return v, nil
}

func int64Value(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an integer", ErrInvalidValue, key)
	}

	return v, nil
}

func boolValue(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%w: %s must be a boolean", ErrInvalidValue, key)
	}

	return v, nil
}

// parseCredentials reads the AUTH_CREDENTIALS value.
//
// The format is a semicolon-separated list of entries, each shaped as
// user_id:key:scope[,scope]. The split is limited to three fields, so a scope
// may itself contain a colon.
//
// A malformed entry is reported by position only. The entry is never echoed,
// because it contains the secret itself.
func parseCredentials(raw string) ([]Credential, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: AUTH_CREDENTIALS", ErrMissingRequired)
	}

	var (
		out  []Credential
		errs []error
	)

	for i, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		parts := strings.SplitN(entry, ":", 3)
		if len(parts) < 2 {
			errs = append(errs, fmt.Errorf("%w: AUTH_CREDENTIALS entry %d has the wrong shape", ErrInvalidValue, i+1))

			continue
		}

		userID := strings.TrimSpace(parts[0])
		key := strings.TrimSpace(parts[1])

		if userID == "" {
			errs = append(errs, fmt.Errorf("%w: AUTH_CREDENTIALS entry %d has an empty user id", ErrInvalidValue, i+1))

			continue
		}

		if key == "" {
			errs = append(errs, fmt.Errorf("%w: AUTH_CREDENTIALS entry %d has an empty key", ErrInvalidValue, i+1))

			continue
		}

		cred := Credential{UserID: userID, Key: Secret{value: key}}

		if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
			cred.Scopes = strings.Split(parts[2], ",")
		}

		out = append(out, cred)
	}

	if len(out) == 0 {
		return nil, errors.Join(append(errs, fmt.Errorf("%w: AUTH_CREDENTIALS", ErrMissingRequired))...)
	}

	return out, errors.Join(errs...)
}
