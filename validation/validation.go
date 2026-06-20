// Package validation is code-based input validation that produces
// field-keyed errors. There are no struct tags and no reflection: a
// type declares its rules in a Validate method using [Field] and
// [Check], so the same type validates whether its values arrive from an
// HTML form, a JSON API, a job payload, or anywhere else.
//
//	func (in LoginInput) Validate() validation.Errors {
//	    return validation.Check(
//	        validation.Field("email",    in.Email,    validation.Required(), validation.Email()),
//	        validation.Field("password", in.Password, validation.Required(), validation.MinLen(8)),
//	    )
//	}
//
// Errors are keyed by field name (the same name a form input uses), so
// a UI can show one message under each field. [Check] returns the
// concrete [Errors] type (a map) rather than the error interface, so
// the valid/invalid test (Empty) never hits the typed-nil trap.
//
// Rule conventions: the format and length rules ([Email], [URL],
// [MinLen], [MaxLen], [Pattern]) treat an empty string as valid, so a
// field is optional unless you also add [Required]. The numeric and set
// rules ([Min], [Max], [In]) have no such exemption — a zero value is a
// real value to them.
package validation

import (
	"sort"
	"strings"
)

// Rule checks a value of type T, returning an error whose message is
// shown to the user when the value is invalid, or nil when it is valid.
// A plain func(T) error is a Rule; see [By] for custom logic.
type Rule[T any] func(value T) error

// Errors maps a field name to its (first) error message. The nil map is
// the valid result. The empty key "" is conventionally a form-level
// error not tied to a single field. Errors implements error so a result
// can also be returned up a call stack.
type Errors map[string]string

// Empty reports whether there are no errors.
func (e Errors) Empty() bool { return len(e) == 0 }

// Has reports whether field has an error.
func (e Errors) Has(field string) bool { _, ok := e[field]; return ok }

// Get returns field's error message, or "" if it has none.
func (e Errors) Get(field string) string { return e[field] }

// Add records msg for field unless field already has an error (first
// wins). Useful for form-level (key "") or cross-field errors added
// outside the rule pipeline. It allocates the map if needed.
func (e *Errors) Add(field, msg string) {
	if *e == nil {
		*e = Errors{}
	}
	if _, ok := (*e)[field]; !ok {
		(*e)[field] = msg
	}
}

// Error implements error, joining "field: message" pairs in sorted
// field order for a stable string. A form-level error (key "") is
// rendered without the leading "field: ".
func (e Errors) Error() string {
	if len(e) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("; ")
		}
		if k != "" {
			b.WriteString(k)
			b.WriteString(": ")
		}
		b.WriteString(e[k])
	}
	return b.String()
}

// Validatable is the contract callers (e.g. the forms package) use to
// validate a value generically.
type Validatable interface {
	Validate() Errors
}

// Constraint is one field's validation, produced by [Field] and run by
// [Check]. It is sealed — construct it only through Field.
type Constraint interface {
	check() (field, message string)
}

type constraint[T any] struct {
	field string
	value T
	rules []Rule[T]
}

func (c constraint[T]) check() (string, string) {
	for _, r := range c.rules {
		if r == nil {
			continue
		}
		if err := r(c.value); err != nil {
			return c.field, err.Error()
		}
	}
	return c.field, ""
}

// Field binds a field name, its value, and the rules to apply. Rules run
// in order and the first failure wins, so list the most fundamental
// rule (usually [Required]) first.
func Field[T any](name string, value T, rules ...Rule[T]) Constraint {
	return constraint[T]{field: name, value: value, rules: rules}
}

// Check runs every constraint and returns the field-keyed errors, or
// nil when all pass.
func Check(constraints ...Constraint) Errors {
	var errs Errors
	for _, c := range constraints {
		field, msg := c.check()
		if msg == "" {
			continue
		}
		if errs == nil {
			errs = Errors{}
		}
		if _, exists := errs[field]; !exists {
			errs[field] = msg
		}
	}
	return errs
}
