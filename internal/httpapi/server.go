// Package httpapi exposes the public booking API.
//
// The transport layer is responsible for the rules AGENTS.md states about a
// public interface: validate input, return uniform errors, never expose an
// internal stack trace, bound request size, rate limit, handle repeated
// requests correctly, return only permitted fields, and keep preliminary data
// visibly distinct from confirmed data.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/auth"
	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/service"
)

// idempotencyHeader is how a client declares that a retry is the same
// operation. It is mandatory for every state-changing endpoint.
const idempotencyHeader = "Idempotency-Key"

// healthPath is the only route reachable without a credential. It reports that
// the process is alive and nothing else, so exposing it costs no information.
const healthPath = "/healthz"

// maxIdempotencyKeyLen bounds the header so it cannot bloat the logs.
const maxIdempotencyKeyLen = 128

// Server holds the dependencies of the API.
type Server struct {
	svc     *service.Service
	auth    *auth.Authenticator
	log     *slog.Logger
	maxBody int64
	limiter *rateLimiter
	mux     *http.ServeMux
	timeNow func() time.Time
}

// Options configures the server.
type Options struct {
	MaxBodyBytes      int64
	RequestsPerMinute int
	Now               func() time.Time
}

// New builds the API server with the full middleware chain applied.
func New(svc *service.Service, authenticator *auth.Authenticator, log *slog.Logger, opts Options) (http.Handler, error) {
	if svc == nil {
		return nil, errors.New("httpapi: service is required")
	}

	if authenticator == nil {
		return nil, errors.New("httpapi: authenticator is required")
	}

	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 64 * 1024
	}

	if opts.RequestsPerMinute <= 0 {
		opts.RequestsPerMinute = 60
	}

	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}

	s := &Server{
		svc:     svc,
		auth:    authenticator,
		log:     log,
		maxBody: opts.MaxBodyBytes,
		limiter: newRateLimiter(opts.RequestsPerMinute, time.Minute, opts.Now),
		timeNow: opts.Now,
		mux:     http.NewServeMux(),
	}

	s.routes()

	// Order matters: the body limit and the panic guard must be in place
	// before anything reads a request or writes a response.
	var handler http.Handler = s.mux
	handler = recoverMiddleware(log)(handler)
	handler = loggingMiddleware(log)(handler)
	handler = maxBodyMiddleware(opts.MaxBodyBytes)(handler)
	handler = requestIDMiddleware(handler)
	handler = rateLimitMiddleware(s.limiter)(handler)
	handler = authenticator.Middleware(healthPath, handler)

	return handler, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET "+healthPath, s.handleHealth)
	s.mux.HandleFunc("POST /v1/bookings", s.handleCreateBooking)
	s.mux.HandleFunc("GET /v1/bookings/{bookingID}", s.handleGetBooking)
	s.mux.HandleFunc("POST /v1/bookings/{bookingID}/search", s.handleSearch)
	s.mux.HandleFunc("POST /v1/bookings/{bookingID}/hold", s.handleHold)
	s.mux.HandleFunc("POST /v1/bookings/{bookingID}/confirm", s.handleConfirm)
	s.mux.HandleFunc("POST /v1/bookings/{bookingID}/cancel", s.handleCancel)
}

// handleHealth reports liveness. It deliberately exposes no dependency detail,
// so a probe cannot be used to fingerprint the deployment.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// createBookingRequest is the wire shape for creating a booking. Unknown
// fields are rejected so a client cannot smuggle a value the service does not
// understand.
type createBookingRequest struct {
	Origin      string             `json:"origin"`
	Destination string             `json:"destination"`
	Departure   string             `json:"departure"`
	Return      string             `json:"return,omitempty"`
	Cabin       string             `json:"cabin"`
	Passengers  []passengerRequest `json:"passengers"`
	Currency    string             `json:"currency"`
}

type passengerRequest struct {
	FullName string `json:"full_name"`
}

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	key := idempotencyKeyFrom(r)
	if key == "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "The Idempotency-Key header is required.")

		return
	}

	var body createBookingRequest

	if err := s.decode(w, r, &body); err != nil {
		fail(w, r, s.log, err)

		return
	}

	req, err := toDomainRequest(body)
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	b, err := s.svc.Create(r.Context(), service.CreateCommand{
		UserID:         userID,
		Request:        req,
		IdempotencyKey: key,
		RequestID:      requestIDFrom(r.Context()),
	})
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	writeJSON(w, http.StatusCreated, toBookingResponse(b))
}

func (s *Server) handleGetBooking(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	b, err := s.svc.Get(r.Context(), r.PathValue("bookingID"), userID)
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	writeJSON(w, http.StatusOK, toBookingResponse(b))
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	if err := s.requireIdempotencyKey(w, r); err != nil {
		fail(w, r, s.log, err)

		return
	}

	offers, err := s.svc.Search(r.Context(), r.PathValue("bookingID"), userID)
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	b, err := s.svc.Get(r.Context(), r.PathValue("bookingID"), userID)
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	writeJSON(w, http.StatusOK, toOffersResponse(b, offers))
}

func (s *Server) handleHold(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	key := idempotencyKeyFrom(r)
	if key == "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "The Idempotency-Key header is required.")

		return
	}

	b, err := s.svc.Hold(r.Context(), service.HoldCommand{
		BookingID:      r.PathValue("bookingID"),
		UserID:         userID,
		IdempotencyKey: key,
	})
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	writeJSON(w, http.StatusOK, toBookingResponse(b))
}

// confirmBookingRequest carries the explicit user decision. The Confirmed
// field is what a real user interface sets when a person presses the confirm
// button; nothing in the AI path may set it.
type confirmBookingRequest struct {
	Confirmed bool `json:"confirmed"`
	// AcceptedAmount and AcceptedCurrency let a client state the price it
	// agreed to. When present they must match the supplier's current price.
	AcceptedAmount   *int64  `json:"accepted_amount,omitempty"`
	AcceptedCurrency *string `json:"accepted_currency,omitempty"`
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	key := idempotencyKeyFrom(r)
	if key == "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "The Idempotency-Key header is required.")

		return
	}

	var body confirmBookingRequest

	if err := s.decode(w, r, &body); err != nil {
		fail(w, r, s.log, err)

		return
	}

	cmd := service.ConfirmCommand{
		BookingID:      r.PathValue("bookingID"),
		UserID:         userID,
		IdempotencyKey: key,
		UserConfirmed:  body.Confirmed,
	}

	if (body.AcceptedAmount == nil) != (body.AcceptedCurrency == nil) {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest,
			"accepted_amount and accepted_currency must be supplied together.")

		return
	}

	if body.AcceptedAmount != nil {
		currency, err := booking.ParseCurrency(*body.AcceptedCurrency)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "accepted_currency is not supported.")

			return
		}

		accepted := booking.Money{Amount: *body.AcceptedAmount, Currency: currency}
		cmd.AcceptedPrice = &accepted
	}

	b, err := s.svc.Confirm(r.Context(), cmd)
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	writeJSON(w, http.StatusOK, toBookingResponse(b))
}

type cancelBookingRequest struct {
	Confirmed bool `json:"confirmed"`
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	key := idempotencyKeyFrom(r)
	if key == "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "The Idempotency-Key header is required.")

		return
	}

	var body cancelBookingRequest

	if err := s.decode(w, r, &body); err != nil {
		fail(w, r, s.log, err)

		return
	}

	b, err := s.svc.Cancel(r.Context(), service.CancelCommand{
		BookingID:      r.PathValue("bookingID"),
		UserID:         userID,
		IdempotencyKey: key,
		UserConfirmed:  body.Confirmed,
	})
	if err != nil {
		fail(w, r, s.log, err)

		return
	}

	writeJSON(w, http.StatusOK, toBookingResponse(b))
}

// principal returns the authenticated user, or writes a 401 and reports false.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.UserID == "" {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "Authentication is required.")

		return "", false
	}

	return p.UserID, true
}

// decode reads a bounded JSON body with unknown fields rejected.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, s.maxBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return invalid(errors.New("request body is not valid JSON"))
		}

		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return invalid(errors.New("request body has a field of the wrong type: " + typeErr.Field))
		}

		return invalid(errors.New("request body could not be parsed"))
	}

	// Reject a second JSON document in the same body.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalid(errors.New("request body must contain a single JSON object"))
	}

	return nil
}

func (s *Server) requireIdempotencyKey(w http.ResponseWriter, r *http.Request) error {
	if idempotencyKeyFrom(r) == "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "The Idempotency-Key header is required.")

		return errors.New("missing idempotency key")
	}

	return nil
}

func idempotencyKeyFrom(r *http.Request) string {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		return ""
	}

	key = truncate(key, maxIdempotencyKeyLen)
	if key == "" {
		return ""
	}

	// Only opaque characters are allowed, so the key cannot be used to inject
	// content into a log line.
	out := make([]byte, 0, len(key))

	for i := range len(key) {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == ':':
			out = append(out, c)
		}
	}

	return string(out)
}

// toDomainRequest converts the wire shape into a validated domain request.
func toDomainRequest(body createBookingRequest) (booking.Request, error) {
	cabin, err := booking.ParseCabin(body.Cabin)
	if err != nil {
		return booking.Request{}, invalid(booking.ErrInvalidCabin)
	}

	currency := booking.CurrencyEUR

	if body.Currency != "" {
		parsed, err := booking.ParseCurrency(body.Currency)
		if err != nil {
			return booking.Request{}, invalid(err)
		}

		currency = parsed
	}

	departure, err := time.Parse("2006-01-02", body.Departure)
	if err != nil {
		return booking.Request{}, invalid(booking.ErrDepartureInPast)
	}

	req := booking.Request{
		Origin:      booking.Location{Code: body.Origin},
		Destination: booking.Location{Code: body.Destination},
		Departure:   departure,
		Cabin:       cabin,
		Currency:    currency,
	}

	if body.Return != "" {
		ret, err := time.Parse("2006-01-02", body.Return)
		if err != nil {
			return booking.Request{}, invalid(booking.ErrReturnBeforeDepart)
		}

		req.Return = &ret
	}

	for _, p := range body.Passengers {
		req.Passengers = append(req.Passengers, booking.Passenger{FullName: p.FullName})
	}

	return req.Normalise(), nil
}
