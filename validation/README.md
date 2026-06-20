# validation

Code-based input validation for Go, producing field-keyed errors. No struct tags, no reflection, no `net/http` — a type declares its rules in a `Validate` method and the same type validates from an HTML form, a JSON API, a job payload, or anywhere else.

## Features

| Feature | What it gives you |
|---|---|
| Code-based, type-safe | Rules are `Rule[T]` values, checked by the compiler — no `validate:"..."` tag DSL. |
| Field-keyed errors | `Errors` is `map[field]message`, so a UI shows one message per input. |
| Type owns its rules | A `Validate() Errors` method works everywhere; no duplication across form/API. |
| No typed-nil trap | `Check` returns the concrete `Errors`; test with `.Empty()`. |
| stdlib only | `regexp`, `cmp`, `net/mail`, `net/url`. |

## Install

```bash
go get github.com/moostackhq/go/validation
```

## Usage

```go
type LoginInput struct {
    Email    string
    Password string
}

func (in LoginInput) Validate() validation.Errors {
    return validation.Check(
        validation.Field("email",    in.Email,    validation.Required(), validation.Email()),
        validation.Field("password", in.Password, validation.Required(), validation.MinLen(8)),
    )
}

errs := input.Validate()
if !errs.Empty() {
    // errs.Get("email") → "must be a valid email address"
}
```

Rules run in order and the **first failure wins** per field, so list the most fundamental rule (usually `Required`) first.

## Rules

Parameterless rules are values; parameterized ones are functions whose type is inferred from their argument — both slot into `Field` without explicit type parameters.

Every rule is a function call, so the API is uniform:

```go
// Rule[string]
validation.Required()                // non-blank (empty or whitespace fails)
validation.Email()
validation.URL()
validation.MinLen(8)                 // rune count
validation.MaxLen(100)
validation.Pattern(re)               // *regexp.Regexp

// inferred from the argument
validation.Min(18)                   // Rule[int] (any cmp.Ordered)
validation.Max(65535)
validation.In("http", "tcp")         // Rule[T comparable]
validation.By(func(s string) error { ... }) // custom / cross-field
```

**Empty-string convention:** the format and length rules (`Email`, `URL`, `MinLen`, `MaxLen`, `Pattern`) treat `""` as valid, so a field is optional unless you also add `Required`. The numeric and set rules (`Min`, `Max`, `In`) have no exemption — a zero value is a real value (and a `NaN` float fails `Min`/`Max`).

`Email` is a practical check, not full RFC 5322: it rejects display-name/comment forms (`Bob <a@b.com>`) and dotless domains (`a@b`, `x@localhost`), which is what a form's email field expects.

**Numeric inference:** `Min`/`Max` infer their type from the argument, so for a non-`int` field match the type — `Min(int32(1))` for an `int32` field, `Min(1.0)` for a `float64`.

## Errors

```go
type Errors map[string]string

func (Errors) Empty() bool
func (Errors) Has(field string) bool
func (Errors) Get(field string) string
func (*Errors) Add(field, msg string)   // first wins; "" = form-level
func (Errors) Error() string            // "email: ...; password: ..." (sorted)
```

The empty key `""` is a form-level error (e.g. "email or password is wrong") added by a handler outside the rule pipeline. `Errors` implements `error` so a result can also be returned up a stack — but for the valid/invalid test use `Empty()`, not `== nil` on an `error`.

## Status

Reference code. Flat structs (nested-struct composition is a planned `Sub` helper); fixed English messages (no i18n layer yet). The `forms` package builds on this for HTTP request binding + repopulation.
