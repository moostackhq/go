// Package mailer composes and sends email. It builds correct MIME
// messages — multipart/alternative for text+HTML, attachments, proper
// encoding — and ships them over a pluggable [Transport]: SMTP for real
// delivery, a log transport for development.
//
// Rendering bodies is the caller's job: render your text/HTML with
// whatever templating you use (e.g. the template package) and set
// [Message.Text] / [Message.HTML]. mailer stays out of templating.
//
//	m := mailer.New(smtp, mailer.WithDefaultFrom("alerts@example.com"))
//	err := m.Send(ctx, &mailer.Message{
//	    To:      []string{"ops@example.com"},
//	    Subject: "Monitor prod is DOWN",
//	    Text:    body, // rendered by the caller
//	})
//
// Send is synchronous; for request paths, send from a background job so a
// slow SMTP server never blocks a handler.
package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// Message is an email to send. At least one of [Message.Text] or
// [Message.HTML] must be set; with both, recipients get a
// multipart/alternative message. From may be empty when the [Mailer] has
// a default sender. Addresses may carry display names ("Bob <b@x.com>").
type Message struct {
	From        string
	ReplyTo     string
	To          []string
	Cc          []string
	Bcc         []string // recipients, but not listed in the headers
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
	Headers     map[string]string // extra headers (e.g. "List-Unsubscribe")
}

// Attachment is a file attached to a [Message].
type Attachment struct {
	Filename    string
	ContentType string // defaults to application/octet-stream
	Content     []byte
}

// Transport delivers a validated message. The package ships SMTP
// ([NewSMTP]) and a dev log transport ([NewLogTransport]); an API-based
// sender (SES, Postmark) implements the same interface.
type Transport interface {
	Send(ctx context.Context, msg *Message) error
}

// Mailer sends messages through a [Transport], applying a default sender.
// Safe for concurrent use if its transport is.
type Mailer struct {
	transport Transport
	from      string
}

// Option configures a [Mailer].
type Option func(*Mailer)

// WithDefaultFrom sets the From used when a [Message] leaves From empty.
func WithDefaultFrom(from string) Option {
	return func(m *Mailer) { m.from = from }
}

// New returns a Mailer over transport.
func New(transport Transport, opts ...Option) *Mailer {
	m := &Mailer{transport: transport}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Validation errors from [Mailer.Send].
var (
	ErrNoRecipients = errors.New("mailer: message has no recipients")
	ErrNoSender     = errors.New("mailer: message has no From and the mailer has no default")
	ErrNoBody       = errors.New("mailer: message has neither Text nor HTML body")
	ErrBadHeader    = errors.New("mailer: invalid custom header")
)

// reservedHeaders are written by render itself; a custom Header with one
// of these names would duplicate or fight the generated value, so they're
// rejected.
var reservedHeaders = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "reply-to": true,
	"subject": true, "date": true, "message-id": true,
	"mime-version": true, "content-type": true, "content-transfer-encoding": true,
}

// Send fills the default From if unset, validates the message, and
// delivers it through the transport. The caller's Message is not mutated.
func (m *Mailer) Send(ctx context.Context, msg *Message) error {
	out := *msg
	if out.From == "" {
		out.From = m.from
	}
	if err := validate(&out); err != nil {
		return err
	}
	return m.transport.Send(ctx, &out)
}

func validate(msg *Message) error {
	if msg.From == "" {
		return ErrNoSender
	}
	if len(msg.To)+len(msg.Cc)+len(msg.Bcc) == 0 {
		return ErrNoRecipients
	}
	if msg.Text == "" && msg.HTML == "" {
		return ErrNoBody
	}
	for _, addr := range allAddresses(msg) {
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("mailer: invalid address %q: %w", addr, err)
		}
	}
	for k, v := range msg.Headers {
		// The key must be a valid RFC 5322 field name: non-empty, printable
		// ASCII, no space/control/colon. This blocks CRLF/NUL injection and
		// leading-space "folds" that would corrupt a preceding header.
		if !validHeaderName(k) {
			return fmt.Errorf("%w %q: not a valid header name", ErrBadHeader, k)
		}
		// The value must carry no control characters that could inject a new
		// header or terminate the header block.
		if strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("%w %q: value contains a control character", ErrBadHeader, k)
		}
		// Names render controls can't be overridden via the map.
		if reservedHeaders[strings.ToLower(k)] {
			return fmt.Errorf("%w %q: reserved, set it via the Message field", ErrBadHeader, k)
		}
	}
	return nil
}

// validHeaderName reports whether k is a valid RFC 5322 field name: a
// non-empty run of printable ASCII (33–126) excluding ':'.
func validHeaderName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if c := k[i]; c < 33 || c > 126 || c == ':' {
			return false
		}
	}
	return true
}

func allAddresses(msg *Message) []string {
	addrs := []string{msg.From}
	if msg.ReplyTo != "" {
		addrs = append(addrs, msg.ReplyTo)
	}
	addrs = append(addrs, msg.To...)
	addrs = append(addrs, msg.Cc...)
	addrs = append(addrs, msg.Bcc...)
	return addrs
}

// recipients returns the envelope recipients (To+Cc+Bcc), address-only.
func recipients(msg *Message) []string {
	all := append(append(append([]string{}, msg.To...), msg.Cc...), msg.Bcc...)
	out := make([]string, 0, len(all))
	for _, a := range all {
		out = append(out, addressOnly(a))
	}
	return out
}

// addressOnly strips a display name, returning the bare address.
func addressOnly(s string) string {
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	return s
}
