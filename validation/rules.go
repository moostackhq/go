package validation

import (
	"cmp"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Required fails on a blank string — empty or whitespace-only. It is the
// only built-in rule that enforces presence; the format and length
// rules pass an empty string so a field can be optional without it.
func Required() Rule[string] { return requiredRule }

var requiredRule Rule[string] = func(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("is required")
	}
	return nil
}

// Email fails on a string that is not a plain email address. It is a
// practical check, not full RFC 5322: it rejects the display-name and
// comment forms mail.ParseAddress also accepts ("Bob <a@b>", "a@b (c)")
// by requiring the parse to equal the input, and requires a dotted
// domain (so "a@b" and "x@localhost" fail). Empty passes (pair with
// [Required] to require it).
func Email() Rule[string] { return emailRule }

var emailRule Rule[string] = func(s string) error {
	if s == "" {
		return nil
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return errors.New("must be a valid email address")
	}
	at := strings.LastIndex(s, "@")
	if at < 0 || !strings.Contains(s[at+1:], ".") {
		return errors.New("must be a valid email address")
	}
	return nil
}

// URL fails on a string that is not an absolute URL (scheme + host).
// Empty passes.
func URL() Rule[string] { return urlRule }

var urlRule Rule[string] = func(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("must be a valid URL")
	}
	return nil
}

// MinLen requires at least n runes. Empty passes (presence is
// [Required]'s job).
func MinLen(n int) Rule[string] {
	return func(s string) error {
		if s == "" {
			return nil
		}
		if utf8.RuneCountInString(s) < n {
			return fmt.Errorf("must be at least %d characters", n)
		}
		return nil
	}
}

// MaxLen requires at most n runes.
func MaxLen(n int) Rule[string] {
	return func(s string) error {
		if utf8.RuneCountInString(s) > n {
			return fmt.Errorf("must be at most %d characters", n)
		}
		return nil
	}
}

// Pattern requires the string to match re. Empty passes. Panics if re
// is nil — fail fast at setup rather than per request.
func Pattern(re *regexp.Regexp) Rule[string] {
	if re == nil {
		panic("validation.Pattern: nil regexp")
	}
	return func(s string) error {
		if s == "" {
			return nil
		}
		if !re.MatchString(s) {
			return errors.New("is not in the expected format")
		}
		return nil
	}
}

// Min requires value >= n (any ordered type). A NaN float value fails,
// rather than slipping through the comparison.
func Min[T cmp.Ordered](n T) Rule[T] {
	return func(v T) error {
		if v != v { // NaN (only reachable for float T)
			return errors.New("must be a valid number")
		}
		if v < n {
			return fmt.Errorf("must be at least %v", n)
		}
		return nil
	}
}

// Max requires value <= n (any ordered type). A NaN float value fails.
func Max[T cmp.Ordered](n T) Rule[T] {
	return func(v T) error {
		if v != v { // NaN
			return errors.New("must be a valid number")
		}
		if v > n {
			return fmt.Errorf("must be at most %v", n)
		}
		return nil
	}
}

// In requires value to be one of allowed.
func In[T comparable](allowed ...T) Rule[T] {
	return func(v T) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", joinValues(allowed))
	}
}

// joinValues renders allowed values as a readable comma-separated list,
// avoiding Go's "[a b c]" slice formatting in user-facing messages.
func joinValues[T any](vals []T) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}

// By adapts an arbitrary func into a Rule — the escape hatch for custom
// or cross-field checks. (A plain func(T) error already satisfies Rule,
// so By is mainly for readability at the call site.)
func By[T any](fn func(T) error) Rule[T] { return fn }
