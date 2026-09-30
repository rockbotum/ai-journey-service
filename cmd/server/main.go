// Command server runs the AI journey booking service.
//
// The binary is deliberately thin: it loads configuration, wires the
// dependencies, starts the listener, and shuts down cleanly. All behaviour
// lives in internal packages, where it can be tested without a socket.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/auth"
	"github.com/rockbotum/ai-journey-service/internal/config"
	"github.com/rockbotum/ai-journey-service/internal/httpapi"
	"github.com/rockbotum/ai-journey-service/internal/logging"
	"github.com/rockbotum/ai-journey-service/internal/provider"
	"github.com/rockbotum/ai-journey-service/internal/service"
	"github.com/rockbotum/ai-journey-service/internal/store"
)

// healthEndpoint is the liveness route served by the API. It is duplicated here
// deliberately: the probe must keep working even if the API package changes,
// otherwise a broken build would also make itself look healthy.
const healthEndpoint = "/healthz"

// healthCheckFlag switches the binary into health-check mode instead of
// starting a server.
var healthCheckFlag = flag.Bool("health-check", false,
	"probe the local health endpoint and exit 0 when healthy, 1 otherwise")

func main() {
	flag.Parse()

	// Container health checking is done by the binary itself, because the
	// runtime image has no shell and no HTTP client to call it with.
	if *healthCheckFlag {
		if err := probeHealth(); err != nil {
			fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)

			os.Exit(1)
		}

		return
	}

	if err := run(); err != nil {
		// The logger may not exist yet, so a startup failure is reported on
		// stderr in plain text.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)

		os.Exit(1)
	}
}

// probeHealth calls the service's own liveness endpoint.
//
// It deliberately exercises the real HTTP path, including authentication
// wiring and middleware, rather than reporting that the process exists: a
// container that is listening but cannot serve is not healthy.
func probeHealth() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get(healthURL(cfg.HTTP.Listen))
	if err != nil {
		return fmt.Errorf("probe %s: %w", healthURL(cfg.HTTP.Listen), err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: status %d", healthURL(cfg.HTTP.Listen), resp.StatusCode)
	}

	return nil
}

// healthURL turns a listen address into a loopback URL. A bare port such as
// ":8080" means "all interfaces", which is not a valid request target.
func healthURL(listen string) string {
	if listen == "" {
		listen = ":8080"
	}

	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8080" + healthEndpoint
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port) + healthEndpoint
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// A misconfigured service must not start in a half-working state.
		return fmt.Errorf("load configuration: %w", err)
	}

	log := newLogger(cfg.Env)
	slog.SetDefault(log)

	log.Info("starting service", "env", cfg.Env, "ai_enabled", cfg.AI.Enabled)

	handler, err := buildHandler(cfg, log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Listen,
		Handler:           handler,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		// A hard limit on header size stops a slowloris from holding a
		// connection open with an oversized header block.
		MaxHeaderBytes: 1 << 20,
	}

	return serve(srv, cfg.HTTP.ShutdownTimeout, log)
}

// newLogger builds the redacting structured logger. Every handler in the
// process receives it, so a secret cannot escape by being logged from a package
// that forgot to be careful.
func newLogger(env string) *slog.Logger {
	return newLoggerInto(env, os.Stdout)
}

// newLoggerInto is the testable core of newLogger: the writer is a parameter so
// a test can assert what the process would have written.
func newLoggerInto(env string, w io.Writer) *slog.Logger {
	level := slog.LevelInfo

	if env == "development" {
		level = slog.LevelDebug
	}

	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})

	return slog.New(logging.NewHandler(base))
}

// buildHandler wires the application graph.
func buildHandler(cfg config.Config, log *slog.Logger) (http.Handler, error) {
	// The store is in-memory for this increment. A production deployment uses
	// the PostgreSQL adapter described in the ADR; the interface is the
	// contract that makes that a drop-in change.
	st := store.NewMemory()

	credentials := make([]auth.Credential, 0, len(cfg.Auth.Credentials))

	for _, c := range cfg.Auth.Credentials {
		credentials = append(credentials, auth.Credential{
			Key:    c.Key.Reveal(),
			UserID: c.UserID,
			Scopes: c.Scopes,
		})
	}

	authenticator, err := auth.New(credentials)
	if err != nil {
		return nil, fmt.Errorf("build authenticator: %w", err)
	}

	supplierClient, err := provider.NewHTTPClient(provider.Options{
		BaseURL:        cfg.Supplier.BaseURL,
		APIKey:         cfg.Supplier.APIKey.Reveal(),
		SupplierID:     cfg.Supplier.SupplierID,
		Timeout:        cfg.Supplier.Timeout,
		MaxRetries:     cfg.Supplier.MaxRetries,
		InitialBackoff: cfg.Supplier.InitialBackoff,
		MaxBackoff:     cfg.Supplier.MaxBackoff,
	})
	if err != nil {
		return nil, fmt.Errorf("build supplier client: %w", err)
	}

	svc, err := service.New(st, supplierClient, log, nil, newBookingID)
	if err != nil {
		return nil, fmt.Errorf("build service: %w", err)
	}

	handler, err := httpapi.New(svc, authenticator, log, httpapi.Options{
		MaxBodyBytes:      cfg.HTTP.MaxBodyBytes,
		RequestsPerMinute: cfg.HTTP.RequestsPerMinute,
	})
	if err != nil {
		return nil, fmt.Errorf("build api: %w", err)
	}

	return handler, nil
}

// newBookingID produces a unique, sortable identifier.
//
// The timestamp prefix makes an identifier readable in a log line and keeps
// identifiers roughly ordered; the random suffix removes any chance of two
// processes minting the same value.
func newBookingID() string {
	var buf [8]byte

	if _, err := rand.Read(buf[:]); err != nil {
		// Without randomness the timestamp alone still separates bookings
		// created in different nanoseconds, which is enough to keep the service
		// running rather than failing a user's request.
		return fmt.Sprintf("bk_%d", time.Now().UTC().UnixNano())
	}

	return fmt.Sprintf("bk_%d_%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(buf[:]))
}

// serve runs the listener until a termination signal, then drains in-flight
// requests within the configured budget.
func serve(srv *http.Server, shutdownTimeout time.Duration, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)

	go func() {
		log.Info("listening", "addr", srv.Addr)

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err

			return
		}

		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received", "budget", shutdownTimeout.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// In-flight requests exceeded the budget. Say so explicitly instead of
		// exiting as if the shutdown had been clean.
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	log.Info("shutdown complete")

	return nil
}
