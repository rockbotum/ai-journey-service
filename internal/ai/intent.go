// Package ai is the boundary between untrusted model output and the rest of
// the service.
//
// AGENTS.md is explicit: model output must pass schema, required-field,
// allowed-value and business-constraint checks, and free text must never be
// used as SQL, a URL, a shell command, HTML, a payment operation or a booking
// state change. This package is the single place where that rule is
// implemented, so a future caller cannot bypass it by parsing JSON directly.
package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
)

// Sentinel errors describing why a model response was rejected. The transport
// layer maps them to a 422 without echoing the model text back to the user.
var (
	ErrEmptyResponse     = errors.New("ai: empty model response")
	ErrMalformedJSON     = errors.New("ai: response is not valid JSON")
	ErrUnknownField      = errors.New("ai: response contains an unexpected field")
	ErrMissingField      = errors.New("ai: response is missing a required field")
	ErrInvalidValue      = errors.New("ai: response contains an invalid value")
	ErrConflictingFields = errors.New("ai: response contradicts itself")
	ErrAssertionTooBold  = errors.New("ai: response asserts an unverified fact")
	ErrTooLarge          = errors.New("ai: response exceeds the allowed size")
)

// MaxResponseBytes bounds how much of a model response is read. A model that
// starts streaming an unbounded body must not be able to exhaust memory.
const MaxResponseBytes = 64 * 1024

// Intent is the only shape the service accepts from a model. It carries
// structured parameters and a set of statements the model believes it cannot
// verify, which the service handles differently from facts.
type Intent struct {
	Origin      string   `json:"origin"`
	Destination string   `json:"destination"`
	Departure   string   `json:"departure"`
	Return      string   `json:"return,omitempty"`
	Cabin       string   `json:"cabin"`
	Travellers  int      `json:"travellers"`
	Currency    string   `json:"currency"`
	Missing     []string `json:"missing,omitempty"`

	// UnverifiedNotes are claims the model wants to show the user. They are
	// stored and displayed as model statements, never as supplier facts, and
	// never interpreted as instructions.
	UnverifiedNotes []string `json:"unverified_notes,omitempty"`
}

// rawIntent is the wire shape. It is separate from Intent so that unknown
// fields can be detected and rejected instead of silently ignored.
type rawIntent struct {
	Origin          *string   `json:"origin"`
	Destination     *string   `json:"destination"`
	Departure       *string   `json:"departure"`
	Return          *string   `json:"return"`
	Cabin           *string   `json:"cabin"`
	Travellers      *int      `json:"travellers"`
	Currency        *string   `json:"currency"`
	Missing         *[]string `json:"missing"`
	UnverifiedNotes *[]string `json:"unverified_notes"`
}

// allowedFields is the closed set of keys the schema accepts.
var allowedFields = map[string]struct{}{
	"origin": {}, "destination": {}, "departure": {}, "return": {},
	"cabin": {}, "travellers": {}, "currency": {},
	"missing": {}, "unverified_notes": {},
}

// ParseIntent converts raw model output into a validated Intent.
//
// Every failure is an error, never a partially populated Intent: a
// half-understood request is exactly the case where a booking must not
// proceed.
func ParseIntent(raw []byte) (Intent, error) {
	if len(raw) == 0 {
		return Intent{}, ErrEmptyResponse
	}

	if len(raw) > MaxResponseBytes {
		return Intent{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(raw))
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return Intent{}, ErrEmptyResponse
	}

	// Strip a markdown fence if the model wrapped the JSON in one. Only the
	// fence is removed; the payload itself is still validated strictly.
	trimmed = stripCodeFence(trimmed)

	if err := checkUnknownFields(trimmed); err != nil {
		return Intent{}, err
	}

	var ri rawIntent

	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&ri); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return Intent{}, fmt.Errorf("%w: field %q has the wrong type", ErrInvalidValue, typeErr.Field)
		}

		return Intent{}, fmt.Errorf("%w: %v", ErrMalformedJSON, sanitiseParseError(err))
	}

	// Reject trailing content: two concatenated objects mean the model did not
	// follow the schema, and the second object may carry injected fields.
	if err := ensureSingleDocument(dec); err != nil {
		return Intent{}, err
	}

	return buildIntent(ri)
}

func buildIntent(ri rawIntent) (Intent, error) {
	origin, err := requiredString(ri.Origin, "origin")
	if err != nil {
		return Intent{}, err
	}

	destination, err := requiredString(ri.Destination, "destination")
	if err != nil {
		return Intent{}, err
	}

	departureRaw, err := requiredString(ri.Departure, "departure")
	if err != nil {
		return Intent{}, err
	}

	departure, err := parseDate(departureRaw, "departure")
	if err != nil {
		return Intent{}, err
	}

	cabinRaw, err := requiredString(ri.Cabin, "cabin")
	if err != nil {
		return Intent{}, err
	}

	cabin, err := booking.ParseCabin(cabinRaw)
	if err != nil {
		return Intent{}, fmt.Errorf("%w: cabin %q", ErrInvalidValue, cabinRaw)
	}

	if ri.Travellers == nil {
		return Intent{}, fmt.Errorf("%w: travellers", ErrMissingField)
	}

	if *ri.Travellers < 1 || *ri.Travellers > 9 {
		return Intent{}, fmt.Errorf("%w: travellers %d is out of range", ErrInvalidValue, *ri.Travellers)
	}

	currency := booking.CurrencyEUR
	if ri.Currency != nil && strings.TrimSpace(*ri.Currency) != "" {
		parsed, err := booking.ParseCurrency(*ri.Currency)
		if err != nil {
			return Intent{}, fmt.Errorf("%w: currency %q", ErrInvalidValue, *ri.Currency)
		}

		currency = parsed
	}

	in := Intent{
		Origin:      strings.ToUpper(strings.TrimSpace(origin)),
		Destination: strings.ToUpper(strings.TrimSpace(destination)),
		Departure:   departure.Format("2006-01-02"),
		Cabin:       string(cabin),
		Travellers:  *ri.Travellers,
		Currency:    string(currency),
	}

	if in.Origin == in.Destination {
		return Intent{}, fmt.Errorf("%w: origin and destination are identical", ErrConflictingFields)
	}

	if ri.Return != nil && strings.TrimSpace(*ri.Return) != "" {
		ret, err := parseDate(*ri.Return, "return")
		if err != nil {
			return Intent{}, err
		}

		if ret.Before(departure) {
			return Intent{}, fmt.Errorf("%w: return is before departure", ErrConflictingFields)
		}

		in.Return = ret.Format("2006-01-02")
	}

	if ri.Missing != nil {
		in.Missing = sanitiseList(*ri.Missing)
	}

	if ri.UnverifiedNotes != nil {
		notes := sanitiseList(*ri.UnverifiedNotes)
		if err := checkNotesAreNotInstructions(notes); err != nil {
			return Intent{}, err
		}

		in.UnverifiedNotes = notes
	}

	return in, nil
}

// ToRequest converts a validated Intent into the domain request. Names are not
// carried over: the model does not collect personal data, and the service asks
// for it separately at the point it is genuinely required.
func (in Intent) ToRequest() (booking.Request, error) {
	departure, err := time.Parse("2006-01-02", in.Departure)
	if err != nil {
		return booking.Request{}, fmt.Errorf("%w: departure", ErrInvalidValue)
	}

	req := booking.Request{
		Origin:      booking.Location{Code: in.Origin},
		Destination: booking.Location{Code: in.Destination},
		Departure:   departure,
		Cabin:       booking.Cabin(in.Cabin),
		Currency:    booking.Currency(in.Currency),
	}

	for range in.Travellers {
		req.Passengers = append(req.Passengers, booking.Passenger{})
	}

	if in.Return != "" {
		ret, err := time.Parse("2006-01-02", in.Return)
		if err != nil {
			return booking.Request{}, fmt.Errorf("%w: return", ErrInvalidValue)
		}

		req.Return = &ret
	}

	// Passengers carry no name yet, so the domain rule requiring a name is
	// intentionally not applied here. The service layer asks for names later
	// and rejects the booking until they are supplied.
	return req, nil
}

// checkUnknownFields walks the raw JSON object keys and rejects anything
// outside the closed schema. A model asked to ignore the schema often complies
// by adding fields, and this is where that attempt is stopped.
func checkUnknownFields(raw string) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedJSON, sanitiseParseError(err))
	}

	for key := range probe {
		if _, ok := allowedFields[key]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownField, key)
		}
	}

	return nil
}

func ensureSingleDocument(dec *json.Decoder) error {
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return fmt.Errorf("%w: unexpected trailing content", ErrMalformedJSON)
	}

	return nil
}

func requiredString(v *string, field string) (string, error) {
	if v == nil {
		return "", fmt.Errorf("%w: %s", ErrMissingField, field)
	}

	trimmed := strings.TrimSpace(*v)
	if trimmed == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrMissingField, field)
	}

	return trimmed, nil
}

func parseDate(raw, field string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)

	parsed, err := time.Parse("2006-01-02", trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be YYYY-MM-DD", ErrInvalidValue, field)
	}

	return parsed, nil
}

// instructionMarkers are phrases that indicate the model is trying to steer
// execution rather than describe a trip. Their presence in a note means the
// note is not a fact statement and must not be shown as one.
var instructionMarkers = []string{
	"ignore previous", "ignore all previous", "disregard the system",
	"you are now", "act as", "new instructions", "system prompt",
	"reveal your", "print your instructions", "execute", "run command",
	"drop table", "select * from", "delete from", "update set",
	"http://", "https://",
}

// checkNotesAreNotInstructions rejects a note that reads as an instruction or a
// payload. The model is not permitted to smuggle an action into a field that
// the user interface renders as an informational statement.
func checkNotesAreNotInstructions(notes []string) error {
	for _, note := range notes {
		lower := strings.ToLower(note)

		for _, marker := range instructionMarkers {
			if strings.Contains(lower, marker) {
				return fmt.Errorf("%w: note attempts to issue an instruction", ErrAssertionTooBold)
			}
		}
	}

	return nil
}

// sanitiseList bounds length and strips control characters, keeping model
// output from smuggling terminal escapes or unbounded strings into the UI.
func sanitiseList(in []string) []string {
	const (
		maxItems = 10
		maxLen   = 300
	)

	out := make([]string, 0, min(len(in), maxItems))

	for _, item := range in {
		if len(out) == maxItems {
			break
		}

		cleaned := strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}

			return r
		}, item)

		cleaned = strings.TrimSpace(cleaned)
		if cleaned == "" {
			continue
		}

		if len(cleaned) > maxLen {
			cleaned = cleaned[:maxLen]
		}

		out = append(out, cleaned)
	}

	return out
}

func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}

	// Drop the opening fence and any language tag.
	idx := strings.IndexByte(s, '\n')
	if idx < 0 {
		return s
	}

	s = s[idx+1:]

	// Drop the closing fence if present.
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}

	return strings.TrimSpace(s)
}

// sanitiseParseError removes the offset-bearing detail from encoding/json
// errors. The message is safe to log, and the offset is useless to an
// operator, but the raw error must never be returned to a caller because it
// quotes the model payload.
func sanitiseParseError(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return "invalid syntax"
	}

	return "invalid structure"
}
