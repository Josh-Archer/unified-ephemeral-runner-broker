package adapter

import "errors"

// ErrCapacityExhausted is the v1 sentinel for a Reserve call rejected because
// the provider has no free runner slot. Return it (wrapping is allowed) so
// callers can fall back to another backend. A generic error is treated as a
// provider failure.
var ErrCapacityExhausted = errors.New("adapter capacity exhausted")

// IsCapacityExhausted reports whether err is or wraps ErrCapacityExhausted.
func IsCapacityExhausted(err error) bool {
	return errors.Is(err, ErrCapacityExhausted)
}
