package password

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Credentials is the (email, password) pair the [Parser] extracts
// from a request body.
type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Parser extracts [Credentials] from a request body. Returns an
// error on malformed input; the provider translates that error
// into [Options.OnFailure].
type Parser func(r *http.Request) (Credentials, error)

// DefaultParser auto-detects the request body shape by
// Content-Type:
//
//   - application/json: decode as {"email": "...", "password": "..."}.
//   - application/x-www-form-urlencoded or multipart/form-data:
//     ParseForm and read "email" + "password" fields.
//   - other / missing Content-Type: return [ErrUnsupportedContentType].
//
// Override [Options.Parser] for custom field names ("username" /
// "passwd") or non-standard body shapes.
func DefaultParser(r *http.Request) (Credentials, error) {
	ct := r.Header.Get("Content-Type")
	// Strip any "; charset=..." suffix.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))

	switch ct {
	case "application/json":
		var c Credentials
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			return Credentials{}, err
		}
		return c, nil
	case "application/x-www-form-urlencoded", "multipart/form-data":
		if err := r.ParseForm(); err != nil {
			return Credentials{}, err
		}
		return Credentials{
			Email:    r.PostFormValue("email"),
			Password: r.PostFormValue("password"),
		}, nil
	default:
		return Credentials{}, ErrUnsupportedContentType
	}
}

// ErrUnsupportedContentType is returned by [DefaultParser] when the
// request's Content-Type isn't one of the recognised body shapes.
var ErrUnsupportedContentType = errors.New("password: unsupported content type")

// ErrInvalidCredentials is the generic auth-failure error the
// provider hands to [Options.OnFailure]. The handler should NOT
// surface different errors for "user not found" vs "wrong
// password" — both leak whether the email is registered. The
// default OnFailure writes a generic 401 regardless of cause.
var ErrInvalidCredentials = errors.New("password: invalid credentials")

// ErrPasswordTooShort is the sentinel returned to [Options.OnFailure]
// when a registration attempt's password is shorter than the
// configured [Options.MinPasswordLength]. The concrete value handed
// to OnFailure is a [*PasswordTooShortError] which satisfies
// errors.Is(err, ErrPasswordTooShort) and carries the configured
// minimum for app-side inspection — without leaking that minimum
// through Error().
//
// Custom OnFailure handlers that surface err.Error() directly will
// see only "password: too short", not "must be at least 12
// characters" — preserving the no-information-disclosure discipline
// the rest of the package follows. Handlers that want to render the
// configured minimum in their UI should pull it from the typed error
// via errors.As:
//
//	var pse *password.PasswordTooShortError
//	if errors.As(err, &pse) {
//	    fmt.Fprintf(w, "min %d characters required", pse.Min)
//	}
var ErrPasswordTooShort = errors.New("password: too short")

// PasswordTooShortError is the concrete error type returned to
// [Options.OnFailure] when the registration password is too short.
// Use errors.As to extract Min if a custom handler needs to render
// the configured minimum length. errors.Is(err, ErrPasswordTooShort)
// also works via the Unwrap chain.
type PasswordTooShortError struct {
	Min int // the configured Options.MinPasswordLength at the time of the failure
}

// Error returns ErrPasswordTooShort.Error() — the configured Min is
// deliberately NOT included so that handlers writing err.Error() to
// the wire do not leak policy details.
func (e *PasswordTooShortError) Error() string { return ErrPasswordTooShort.Error() }

// Unwrap exposes ErrPasswordTooShort so errors.Is(err,
// ErrPasswordTooShort) returns true.
func (e *PasswordTooShortError) Unwrap() error { return ErrPasswordTooShort }
