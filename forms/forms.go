// Package forms binds an HTTP request into a typed struct, validates
// it, and exposes per-field errors plus the raw submitted values so a
// failed form re-renders with the user's input intact.
//
//	form, err := forms.Bind[LoginInput](r)
//	if err != nil {                 // request couldn't be parsed → 400
//	    http.Error(w, "bad request", http.StatusBadRequest); return
//	}
//	if !form.Valid() {              // field-level problems → re-render
//	    render(w, "login", page{Form: form}); return
//	}
//	use(form.Data)                  // decoded + validated
//
// Two-level errors: the returned error means the request itself
// couldn't be read (oversized body, malformed multipart, JSON syntax
// error); [Form.Errors] holds expected user-input problems (type
// conversion and validation), which you re-render.
//
// Sources: GET/HEAD use the query string; form-encoded and multipart
// POSTs use the body; application/json decodes the body. Field names
// come from a `form:"..."` tag or the snake_case of the field name
// (matching the sqlx convention); unknown submitted fields (e.g.
// csrf_token) are ignored. If the bound type implements
// [validation.Validatable], Bind runs it and merges the result.
//
// Validation is HTTP-free; this package is the only HTTP-aware layer.
package forms

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/moostackhq/go/validation"
)

// ErrBodyTooLarge is returned by [Bind] when the request body exceeds
// the configured limit (see [WithMaxBytes]).
var ErrBodyTooLarge = errors.New("forms: request body too large")

const (
	defaultMaxBytes  = 10 << 20 // 10 MiB
	defaultMaxMemory = 10 << 20 // 10 MiB multipart in-memory threshold
)

// Form is the result of [Bind]: the decoded struct, the field-keyed
// errors, and (internally) the raw submitted values for repopulation.
type Form[T any] struct {
	Data   T
	Errors validation.Errors
	raw    url.Values
}

// Valid reports whether there were no decode or validation errors.
func (f *Form[T]) Valid() bool { return f.Errors.Empty() }

// Value returns the raw submitted value for a field — what the user
// typed, for repopulating the input (so an "abc" rejected by an int
// field still shows "abc", not 0).
func (f *Form[T]) Value(field string) string { return f.raw.Get(field) }

// Error returns the field's error message, or "".
func (f *Form[T]) Error(field string) string { return f.Errors.Get(field) }

type config struct {
	maxBytes  int64
	maxMemory int64
}

// Option configures [Bind].
type Option func(*config)

// WithMaxBytes caps the request body (default 10 MiB). The limit is
// approximate (off by at most one byte) and surfaces as
// [ErrBodyTooLarge].
//
// One stdlib caveat: ParseForm imposes its own 10 MiB ceiling on
// urlencoded bodies that it only lifts for *http.maxBytesReader, so a
// value above 10 MiB is silently truncated (not an error) on the
// urlencoded path. Multipart and JSON honor the full limit.
func WithMaxBytes(n int64) Option { return func(c *config) { c.maxBytes = n } }

// WithMaxMemory sets the multipart in-memory threshold before file
// parts spill to disk (default 10 MiB).
func WithMaxMemory(n int64) Option { return func(c *config) { c.maxMemory = n } }

// Bind decodes r into a Form[T], validating T if it implements
// [validation.Validatable].
func Bind[T any](r *http.Request, opts ...Option) (*Form[T], error) {
	cfg := config{maxBytes: defaultMaxBytes, maxMemory: defaultMaxMemory}
	for _, o := range opts {
		o(&cfg)
	}
	if r.Body != nil {
		r.Body = &limitReader{r: r.Body, remaining: cfg.maxBytes + 1}
	}

	form := &Form[T]{raw: url.Values{}}

	if mediaType(r) == "application/json" {
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&form.Data); err != nil {
				if errors.Is(err, ErrBodyTooLarge) {
					return nil, ErrBodyTooLarge
				}
				return nil, fmt.Errorf("forms: decode json: %w", err)
			}
		}
	} else {
		values, err := requestValues(r, cfg)
		if err != nil {
			return nil, err
		}
		form.raw = values
		decode(values, &form.Errors, &form.Data)
	}

	// Merge validation. Add is first-wins, so decode (type) errors take
	// precedence over a validation error on the same field.
	if v, ok := any(&form.Data).(validation.Validatable); ok {
		for field, msg := range v.Validate() {
			form.Errors.Add(field, msg)
		}
	}
	return form, nil
}

// requestValues parses the submitted fields from a non-JSON request.
func requestValues(r *http.Request, cfg config) (url.Values, error) {
	if mediaType(r) == "multipart/form-data" {
		if err := r.ParseMultipartForm(cfg.maxMemory); err != nil {
			return nil, parseErr(err)
		}
		if r.MultipartForm != nil {
			return url.Values(r.MultipartForm.Value), nil
		}
		return url.Values{}, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, parseErr(err)
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return r.Form, nil
	}
	return r.PostForm, nil
}

func parseErr(err error) error {
	if errors.Is(err, ErrBodyTooLarge) {
		return ErrBodyTooLarge
	}
	return fmt.Errorf("forms: parse request: %w", err)
}

func mediaType(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return mt
}

// limitReader caps total bytes read, returning ErrBodyTooLarge once the
// limit is passed. remaining starts at maxBytes+1, so a body of exactly
// maxBytes reads cleanly (including the trailing EOF probe).
type limitReader struct {
	r         io.ReadCloser
	remaining int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, ErrBodyTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}

func (l *limitReader) Close() error { return l.r.Close() }
