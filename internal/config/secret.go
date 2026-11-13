package config

import (
	"encoding/json"
	"log/slog"
)

// Redaction markers. A secret's value is never rendered; only whether it is set.
const (
	RedactedSet   = "<set>"
	RedactedUnset = "<unset>"
)

// Secret is a credential. Every rendering path (fmt, %#v, slog, JSON, YAML)
// prints a redaction marker instead of the value. Use Reveal to read it.
type Secret string

// Reveal returns the secret value. Call it only at the point of use.
func (s Secret) Reveal() string { return string(s) }

// IsSet reports whether the secret has a non-empty value.
func (s Secret) IsSet() bool { return s != "" }

func (s Secret) redacted() string {
	if s.IsSet() {
		return RedactedSet
	}
	return RedactedUnset
}

// String implements fmt.Stringer.
func (s Secret) String() string { return s.redacted() }

// GoString implements fmt.GoStringer so %#v is redacted too.
func (s Secret) GoString() string { return s.redacted() }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.redacted()) }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.redacted()) }

// MarshalYAML implements yaml.Marshaler. Decoding is unaffected: yaml.v3
// decodes a scalar into the underlying string.
func (s Secret) MarshalYAML() (any, error) { return s.redacted(), nil }
