package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/rockbotum/ai-journey-service/internal/ai"
	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/provider"
	"github.com/rockbotum/ai-journey-service/internal/service"
	"github.com/rockbotum/ai-journey-service/internal/store"
)

// errorCode is a stable, machine-readable identifier. Clients switch on these,
// never on the human-readable message.
type errorCode string

const (
	codeInvalidRequest   errorCode = "invalid_request"
	codeUnauthorized     errorCode = "unauthorized"
	codeNotFound         errorCode = "not_found"
	codeConflict         errorCode = "conflict"
	codeConsentRequired  errorCode = "user_confirmation_required"
	codePriceChanged     errorCode = "price_changed"
	codeHoldExpired      errorCode = "hold_expired"
	codeOfferUnavailable errorCode = "offer_unavailable"
	codeInvalidState     errorCode = "invalid_state"
	codeRateLimited      errorCode = "rate_limited"
	codePayloadTooLarge  errorCode = "payload_too_large"
	codeUpstream         errorCode = "upstream_unavailable"
	codeInternal         errorCode = "internal_error"
)

// errorBody is the uniform error envelope. AGENTS.md forbids leaking internal
// stack traces, so only the code and a short message are ever returned.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      errorCode `json:"code"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id,omitempty"`
}

// writeJSON sends a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if payload == nil {
		return
	}

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already written, so the only remaining action is
		// to stop. The encoder failure is recorded by the caller's logger.
		return
	}
}

// writeError sends a uniform error response.
func writeError(w http.ResponseWriter, r *http.Request, status int, code errorCode, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestIDFrom(r.Context()),
	}})
}

// invalidRequestError wraps a domain or decode validation failure. The
// transport maps the wrapper to 400 instead of enumerating every domain
// sentinel, and the domain error stays intact for errors.Is checks.
type invalidRequestError struct {
	err error
}

func (e invalidRequestError) Error() string { return e.err.Error() }

func (e invalidRequestError) Unwrap() error { return e.err }

// invalid marks err as a caller-correctable problem.
func invalid(err error) error {
	return invalidRequestError{err: err}
}

// isInvalidRequest reports whether err is a caller-correctable problem.
func isInvalidRequest(err error) bool {
	var target invalidRequestError

	return errors.As(err, &target)
}

// classifyError maps a domain, service or provider error to a status code, a
// stable code and a message that is safe to show to a caller.
//
// The mapping is exhaustive on purpose: an error that does not match any rule
// becomes a generic 500 rather than being reflected to the client.
func classifyError(err error) (status int, code errorCode, message string) {
	switch {
	case err == nil:
		return http.StatusOK, "", ""

	case isInvalidRequest(err):
		return http.StatusBadRequest, codeInvalidRequest, "The request is not valid."

	// A malformed travel request is a caller error even when the service, not
	// the transport, detected it. These are enumerated so a validation failure
	// can never be reported as a server fault.
	case errors.Is(err, booking.ErrOriginRequired),
		errors.Is(err, booking.ErrDestinationRequired),
		errors.Is(err, booking.ErrSameOriginDest),
		errors.Is(err, booking.ErrDepartureInPast),
		errors.Is(err, booking.ErrReturnBeforeDepart),
		errors.Is(err, booking.ErrInvalidSegmentCount),
		errors.Is(err, booking.ErrCabinRequired),
		errors.Is(err, booking.ErrInvalidCabin),
		errors.Is(err, booking.ErrPassengerNameReq),
		errors.Is(err, booking.ErrPassengerCountMax),
		errors.Is(err, booking.ErrInvalidCurrency),
		errors.Is(err, booking.ErrCurrencyMismatch):
		return http.StatusBadRequest, codeInvalidRequest, "The travel request is not valid."

	case errors.Is(err, service.ErrConsentRequired), errors.Is(err, booking.ErrUserConfirmationRequired):
		return http.StatusPreconditionRequired, codeConsentRequired,
			"This action requires an explicit confirmation."

	case errors.Is(err, service.ErrPriceReconfirm), errors.Is(err, booking.ErrPriceChanged):
		return http.StatusConflict, codePriceChanged,
			"The price changed. Review it and confirm again."

	case errors.Is(err, booking.ErrHoldExpired):
		return http.StatusConflict, codeHoldExpired,
			"The hold expired and the option is no longer reserved."

	case errors.Is(err, service.ErrOfferNotAvailable), errors.Is(err, booking.ErrAvailabilityLost),
		errors.Is(err, provider.ErrPriceUnavailable):
		return http.StatusConflict, codeOfferUnavailable,
			"The selected option is no longer available."

	case errors.Is(err, service.ErrInvalidState):
		return http.StatusConflict, codeInvalidState,
			"The operation is not allowed in the current state."

	case errors.Is(err, service.ErrBookingNotFound), errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, codeNotFound, "Booking not found."

	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrIdempotencyConflict):
		return http.StatusConflict, codeConflict,
			"The request conflicts with a concurrent operation."

	case errors.Is(err, provider.ErrRateLimited):
		return http.StatusServiceUnavailable, codeUpstream, "The supplier is busy. Try again shortly."

	case errors.Is(err, provider.ErrUnavailable), errors.Is(err, provider.ErrDeadlineExceeded):
		return http.StatusServiceUnavailable, codeUpstream, "The supplier is temporarily unavailable."

	case errors.Is(err, provider.ErrMalformedResponse):
		// A contract violation is a defect on our side of the integration, so
		// the client is told nothing beyond the fact it must retry.
		return http.StatusBadGateway, codeUpstream, "The supplier returned an unusable response."

	case errors.Is(err, provider.ErrRejected):
		return http.StatusUnprocessableEntity, codeInvalidRequest, "The supplier rejected the request."

	case errors.Is(err, ai.ErrEmptyResponse), errors.Is(err, ai.ErrMalformedJSON),
		errors.Is(err, ai.ErrUnknownField), errors.Is(err, ai.ErrMissingField),
		errors.Is(err, ai.ErrInvalidValue), errors.Is(err, ai.ErrConflictingFields),
		errors.Is(err, ai.ErrAssertionTooBold), errors.Is(err, ai.ErrTooLarge):
		return http.StatusUnprocessableEntity, codeInvalidRequest,
			"The request could not be understood."

	default:
		return http.StatusInternalServerError, codeInternal, "An unexpected error occurred."
	}
}

// fail classifies err, logs it with safe context and writes the response. The
// err attribute goes through the redacting logger, so a message that happens
// to contain personal data is still masked.
func fail(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	status, code, message := classifyError(err)

	if status >= http.StatusInternalServerError {
		log.ErrorContext(r.Context(), "request failed",
			"code", string(code),
			"err", err,
			"request_id", requestIDFrom(r.Context()),
		)
	} else {
		log.WarnContext(r.Context(), "request rejected",
			"code", string(code),
			"request_id", requestIDFrom(r.Context()),
		)
	}

	writeError(w, r, status, code, message)
}
