# forms

HTTP request binding for Go: decode a request into a typed struct, validate it (via [`validation`](../validation)), and re-render a failed form with per-field errors and the user's input intact.

## Features

| Feature | What it gives you |
|---|---|
| Typed binding | `Bind[T]` decodes form / multipart / query / JSON into `T`. |
| Field-keyed errors | Conversion and validation errors keyed by field, for one message per input. |
| Repopulation | `Value(field)` returns the **raw** submission, so a bad `"abc"` in an int field redraws as `"abc"`, not `0`. |
| Two-level errors | An `error` for unparseable requests (400/413); `Form.Errors` for user-input problems (re-render). |
| Validation built in | If `T` implements `validation.Validatable`, `Bind` runs it and merges the result. |

## Install

```bash
go get github.com/moostackhq/go/forms
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

func handleLogin(w http.ResponseWriter, r *http.Request) {
    form, err := forms.Bind[LoginInput](r)
    if err != nil {                  // request couldn't be parsed
        http.Error(w, "bad request", http.StatusBadRequest); return
    }
    if !form.Valid() {               // field errors → re-render
        render(w, "login", page{Form: form}); return
    }
    authenticate(form.Data.Email, form.Data.Password)
}
```

In the template (data-based, no per-request FuncMap):

```html
<input name="email" value="{{ .Form.Value "email" }}">
{{ with .Form.Error "email" }}<span class="err">{{ . }}</span>{{ end }}
```

## Binding rules

- **Sources**: `GET`/`HEAD` → query; form-encoded & multipart `POST` → body; `application/json` → body. Detected by method + Content-Type.
- **Field names**: a `form:"name"` tag, else snake_case of the field (matching the `sqlx` mapping, so one struct maps the same for binding and persistence). `form:"-"` skips a field; unknown submitted fields (e.g. `csrf_token`) are ignored.
- **Types**: `string`, the int/uint/float kinds, `bool`, `[]string`, and pointers to scalars. A `bool` binds `on`/`true`/`1`/`yes` → true (a missing checkbox → false). A **pointer** field is `nil` when the input is absent or blank, set otherwise — modelling "optional vs zero". Unsupported kinds (nested structs, maps, `time.Time`) are left at their zero value in v1.
- **Conversion errors** (e.g. `"abc"` into an `int`) become field errors and the raw value is kept for repopulation; the typed field stays zero.

## Errors

```go
form, err := forms.Bind[T](r)
// err != nil      → unparseable request (oversized body, malformed multipart, JSON syntax). 400/413.
// !form.Valid()   → conversion + validation errors in form.Errors. Re-render.
```

`form.Errors` is a `validation.Errors`. Decode (type) errors take precedence over a validation error on the same field.

## Options

```go
forms.WithMaxBytes(1 << 20)   // cap the body (default 10 MiB) → ErrBodyTooLarge
forms.WithMaxMemory(8 << 20)  // multipart in-memory threshold (default 10 MiB)
```

`ParseForm` enforces its own 10 MiB ceiling on urlencoded bodies, so a `WithMaxBytes` above 10 MiB is silently truncated on that path; multipart and JSON honor the full limit.

## JSON

JSON decodes the whole body via `encoding/json` (using `json:` tags), then runs validation. Because it isn't field-by-field, a type mismatch surfaces as a single `error` (→ 400), not a per-field error, and there's no repopulation — which is fine, since repopulation is an HTML-form concern.

## Status

Reference code. File uploads aren't bound to fields in v1 — after `Bind`, read them via `r.FormFile`. No nested structs / `time.Time` binding yet.
