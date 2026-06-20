# mailer

Compose and send email in Go. It builds correct MIME messages and ships them over a pluggable transport — SMTP for delivery, a log transport for development.

## Scope

mailer does composition + transport. **Rendering bodies is yours**: render your text/HTML with whatever templating you use and set `Message.Text` / `Message.HTML`. mailer stays out of templating, so it composes with the `template` package (or anything else) instead of dictating one.

## Usage

```go
smtp, _ := mailer.NewSMTP("smtp.example.com:587", mailer.WithAuth(user, pass))
m := mailer.New(smtp, mailer.WithDefaultFrom("alerts@example.com"))

err := m.Send(ctx, &mailer.Message{
    To:      []string{"ops@example.com"},
    Subject: "Monitor prod is DOWN",
    Text:    textBody,        // rendered by you
    HTML:    htmlBody,        // optional; both → multipart/alternative
})
```

`Send` is **synchronous**. On a request path, send from a background job (the `jobs` package) so a slow SMTP server never blocks a handler.

## Message

```go
type Message struct {
    From, ReplyTo  string
    To, Cc, Bcc    []string   // Bcc recipients get the mail but aren't in the headers
    Subject        string
    Text, HTML     string     // at least one required
    Attachments    []Attachment
    Headers        map[string]string
}
```

`Send` validates before handing a copy to the transport (your `Message` is never mutated): recipients present, a body present, a sender (falling back to `WithDefaultFrom`), every address parseable, and every custom `Headers` entry safe — a key/value containing `\r`, `\n`, or `\x00` is rejected (header-injection guard), as is any name `render` controls (`Content-Type`, `From`, `Subject`, …) so the map can't duplicate or fight a generated header.

## MIME, done right

The value here is the fiddly part: `text` only → `text/plain`; `html` only → `text/html`; both → `multipart/alternative`; add attachments → `multipart/mixed` wrapping the body. Text uses quoted-printable, attachments base64 (76-col wrapped), the subject is RFC 2047-encoded, display names are encoded, and `Bcc` is kept out of the headers — all stdlib (`mime/multipart`, `net/mail`, `encoding/base64`), zero dependencies.

## Transports

```go
mailer.NewSMTP("host:port", mailer.WithAuth(user, pass)) // STARTTLS on by default
mailer.NewLogTransport(logger)                            // dev: logs instead of sending
```

- **SMTP** — stdlib `net/smtp`; dials with the call's context, upgrades to STARTTLS when the server offers it, authenticates if credentials are given. Works with SES/SendGrid/Mailgun SMTP relays. STARTTLS is **opportunistic** by default — a network attacker who strips the server's advertisement causes a plaintext send — so for any relay across an untrusted network use `WithRequireTLS()`, which fails the send if STARTTLS isn't offered. (Credentials are safe regardless: stdlib `PlainAuth` refuses an unencrypted non-localhost connection.)
- **Log** — the development backend: logs each message at Info (body at Debug) instead of sending, so you develop with no SMTP server. The in-memory-store equivalent.

A future API sender (SES, Postmark) implements the same `Transport` interface — no change to `Mailer` or your call sites.

## Status

Reference code. Standard library only. Header folding for very long recipient lists isn't implemented (rare); everything else follows RFC 5322 / MIME.
