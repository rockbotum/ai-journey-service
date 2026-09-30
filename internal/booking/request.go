package booking

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors for malformed travel requests. They are validation errors, not
// domain transitions, and the API maps them to 400 without leaking internals.
var (
	ErrOriginRequired      = errors.New("booking: origin is required")
	ErrDestinationRequired = errors.New("booking: destination is required")
	ErrSameOriginDest      = errors.New("booking: origin and destination must differ")
	ErrDepartureInPast     = errors.New("booking: departure date is in the past")
	ErrInvalidSegmentCount = errors.New("booking: traveller count must be between 1 and 9")
	ErrCabinRequired       = errors.New("booking: cabin class is required")
	ErrInvalidCabin        = errors.New("booking: unknown cabin class")
	ErrReturnBeforeDepart  = errors.New("booking: return date is before departure date")
	ErrPassengerNameReq    = errors.New("booking: passenger name is required")
	ErrPassengerCountMax   = errors.New("booking: too many passengers")
)

// Cabin is the travel class. It is a closed enum: an unknown value from a
// model or a client is rejected instead of being defaulted.
type Cabin string

const (
	CabinEconomy  Cabin = "economy"
	CabinPremium  Cabin = "premium_economy"
	CabinBusiness Cabin = "business"
	CabinFirst    Cabin = "first"
)

var knownCabins = map[Cabin]struct{}{
	CabinEconomy:  {},
	CabinPremium:  {},
	CabinBusiness: {},
	CabinFirst:    {},
}

// ParseCabin validates a cabin class coming from a client or a model.
func ParseCabin(raw string) (Cabin, error) {
	c := Cabin(strings.ToLower(strings.TrimSpace(raw)))
	if _, ok := knownCabins[c]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidCabin, raw)
	}

	return c, nil
}

// Location is a normalised origin or destination. Codes are stored uppercase
// and trimmed so that " led " and "LED" cannot produce two distinct search
// keys for the same city.
type Location struct {
	Code string `json:"code"`
	Name string `json:"name,omitempty"`
}

func (l Location) validate(field string, err error) error {
	code := strings.ToUpper(strings.TrimSpace(l.Code))
	if code == "" {
		return err
	}

	return nil
}

// Normalised returns the location with a trimmed, uppercased code.
func (l Location) Normalised() Location {
	l.Code = strings.ToUpper(strings.TrimSpace(l.Code))
	l.Name = strings.TrimSpace(l.Name)

	return l
}

// Passenger is one traveller. Only the minimum needed to search and book is
// collected; identity documents are out of scope for the search phase and must
// never be inferred by a model.
type Passenger struct {
	FullName string `json:"full_name"`
}

// Validate checks the passenger list without echoing sensitive values into the
// error message.
func (p Passenger) Validate() error {
	if strings.TrimSpace(p.FullName) == "" {
		return ErrPassengerNameReq
	}

	return nil
}

// Request is the structured intent extracted from a natural-language message.
// It is the only shape the booking domain accepts; free text never reaches it.
type Request struct {
	Origin      Location    `json:"origin"`
	Destination Location    `json:"destination"`
	Departure   time.Time   `json:"departure"`
	Return      *time.Time  `json:"return,omitempty"`
	Cabin       Cabin       `json:"cabin"`
	Passengers  []Passenger `json:"passengers"`
	Currency    Currency    `json:"currency"`
	// RawQuery is retained for audit of how intent was formed. It is stored
	// with restricted access and never sent to a supplier.
	RawQuery string `json:"raw_query,omitempty"`
}

// Validate applies business constraints. now is passed explicitly so the rule
// is testable and so a retry cannot be validated against a moving clock.
func (r Request) Validate() error {
	if err := r.Origin.validate("origin", ErrOriginRequired); err != nil {
		return err
	}

	if err := r.Destination.validate("destination", ErrDestinationRequired); err != nil {
		return err
	}

	if r.Origin.Normalised().Code == r.Destination.Normalised().Code {
		return ErrSameOriginDest
	}

	if r.Departure.IsZero() {
		return ErrDepartureInPast
	}

	if r.Return != nil && r.Return.Before(r.Departure) {
		return ErrReturnBeforeDepart
	}

	if _, ok := knownCabins[r.Cabin]; !ok {
		return ErrInvalidCabin
	}

	if len(r.Passengers) == 0 {
		return ErrInvalidSegmentCount
	}

	if len(r.Passengers) > 9 {
		return ErrPassengerCountMax
	}

	for _, p := range r.Passengers {
		if err := p.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// ValidateAt validates the request against a reference time and additionally
// rejects a departure that has already passed.
func (r Request) ValidateAt(now time.Time) error {
	if err := r.Validate(); err != nil {
		return err
	}

	departure := r.Departure.UTC()
	today := now.UTC().Truncate(24 * time.Hour)

	if departure.Before(today) {
		return ErrDepartureInPast
	}

	return nil
}

// Normalise returns a copy with canonical casing and UTC dates.
func (r Request) Normalise() Request {
	r.Origin = r.Origin.Normalised()
	r.Destination = r.Destination.Normalised()
	r.Cabin = Cabin(strings.ToLower(strings.TrimSpace(string(r.Cabin))))
	r.Currency = Currency(strings.ToUpper(strings.TrimSpace(string(r.Currency))))

	if !r.Departure.IsZero() {
		r.Departure = r.Departure.UTC()
	}

	if r.Return != nil {
		ret := r.Return.UTC()
		r.Return = &ret
	}

	for i := range r.Passengers {
		r.Passengers[i].FullName = strings.TrimSpace(r.Passengers[i].FullName)
	}

	return r
}

// TravellerCount is a safe accessor for logging; it reveals no personal data.
func (r Request) TravellerCount() int { return len(r.Passengers) }
