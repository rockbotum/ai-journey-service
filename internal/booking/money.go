package booking

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Currency is an ISO 4217 alphabetic code. Only currencies the service
// actually prices offers in are modelled; an unknown code is a data error,
// never a silent default.
type Currency string

const (
	CurrencyEUR Currency = "EUR"
	CurrencyUSD Currency = "USD"
	CurrencyRUB Currency = "RUB"
)

var supportedCurrencies = map[Currency]struct{}{
	CurrencyEUR: {},
	CurrencyUSD: {},
	CurrencyRUB: {},
}

// ErrInvalidCurrency is returned for codes outside the supported set.
var ErrInvalidCurrency = errors.New("booking: unsupported currency")

// CurrencyExponent is the number of minor units per major unit for the
// currencies the service supports. All of them use 2, but the mapping is
// explicit so adding a zero-decimal currency later cannot silently break
// arithmetic.
var CurrencyExponent = map[Currency]int{
	CurrencyEUR: 2,
	CurrencyUSD: 2,
	CurrencyRUB: 2,
}

// ParseCurrency validates and normalises a currency code.
func ParseCurrency(raw string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(raw)))
	if _, ok := supportedCurrencies[c]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, raw)
	}

	return c, nil
}

// Money is a monetary amount in minor units (cents, kopecks). Floating point
// is never used for money in this service: comparing, summing and applying
// percentage discounts all happen on int64 minor units.
type Money struct {
	Amount   int64    `json:"amount"`
	Currency Currency `json:"currency"`
}

// Zero returns a zero amount in the given currency.
func Zero(c Currency) Money {
	return Money{Amount: 0, Currency: c}
}

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.Amount == 0 }

// IsNegative reports whether the amount is below zero.
func (m Money) IsNegative() bool { return m.Amount < 0 }

// SameCurrency reports whether both amounts use the same currency. Comparing
// amounts across currencies is a bug, not a conversion opportunity.
func (m Money) SameCurrency(other Money) bool { return m.Currency == other.Currency }

// ErrCurrencyMismatch is returned when arithmetic mixes currencies.
var ErrCurrencyMismatch = errors.New("booking: currency mismatch")

// ErrAmountOverflow is returned when an arithmetic result would overflow int64.
var ErrAmountOverflow = errors.New("booking: amount overflow")

// Add sums two amounts of the same currency.
func (m Money) Add(other Money) (Money, error) {
	if !m.SameCurrency(other) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.Currency, other.Currency)
	}

	sum := m.Amount + other.Amount
	if (sum > m.Amount) != (other.Amount > 0) {
		return Money{}, ErrAmountOverflow
	}

	return Money{Amount: sum, Currency: m.Currency}, nil
}

// Negate returns the amount with the opposite sign, which is how refunds and
// payables are represented. Negating math.MinInt64 is refused.
func (m Money) Negate() (Money, error) {
	if m.Amount == -m.Amount && m.Amount != 0 {
		return Money{}, ErrAmountOverflow
	}

	return Money{Amount: -m.Amount, Currency: m.Currency}, nil
}

// String renders the amount for logs and API responses. It is derived from
// minor units, so no rounding decision is made at the presentation layer.
func (m Money) String() string {
	exp, ok := CurrencyExponent[m.Currency]
	if !ok {
		return strconv.FormatInt(m.Amount, 10) + " " + string(m.Currency)
	}

	sign := ""
	amount := m.Amount
	if amount < 0 {
		sign, amount = "-", -amount
	}

	div := int64(1)
	for range exp {
		div *= 10
	}

	return sign + strconv.FormatInt(amount/div, 10) + "." +
		padZero(strconv.FormatInt(amount%div, 10), exp) + " " + string(m.Currency)
}

func padZero(s string, width int) string {
	for len(s) < width {
		s = "0" + s
	}

	return s
}
