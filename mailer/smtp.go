package mailer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
)

// SMTPTransport delivers messages over SMTP using stdlib net/smtp. It
// dials with the call's context (so a dead server can't hang forever),
// upgrades to STARTTLS when the server offers it, and authenticates if
// credentials were supplied.
type SMTPTransport struct {
	addr       string // host:port
	host       string // for TLS server name + auth
	auth       smtp.Auth
	starttls   bool
	requireTLS bool
}

// SMTPOption configures an [SMTPTransport].
type SMTPOption func(*SMTPTransport)

// WithAuth enables PLAIN authentication with the given credentials.
func WithAuth(username, password string) SMTPOption {
	return func(t *SMTPTransport) {
		t.auth = smtp.PlainAuth("", username, password, t.host)
	}
}

// WithoutSTARTTLS disables the opportunistic STARTTLS upgrade. Use only
// for a local relay on a trusted network; never across the internet.
func WithoutSTARTTLS() SMTPOption {
	return func(t *SMTPTransport) { t.starttls = false; t.requireTLS = false }
}

// WithRequireTLS fails the send if the server does not offer STARTTLS,
// closing the silent-downgrade gap: by default STARTTLS is opportunistic,
// so a network attacker who strips the server's advertisement causes a
// plaintext send. Use this for any relay reached across an untrusted
// network.
func WithRequireTLS() SMTPOption {
	return func(t *SMTPTransport) { t.starttls = true; t.requireTLS = true }
}

// NewSMTP returns an SMTP transport for addr ("host:port"). STARTTLS is
// on by default; add [WithAuth] for authenticated relays.
func NewSMTP(addr string, opts ...SMTPOption) (*SMTPTransport, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("mailer: invalid SMTP address %q: %w", addr, err)
	}
	t := &SMTPTransport{addr: addr, host: host, starttls: true}
	for _, o := range opts {
		o(t)
	}
	return t, nil
}

// Send implements [Transport].
func (t *SMTPTransport) Send(ctx context.Context, msg *Message) error {
	raw, err := render(msg)
	if err != nil {
		return err
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fmt.Errorf("mailer: dial %s: %w", t.addr, err)
	}
	c, err := smtp.NewClient(conn, t.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: smtp: %w", err)
	}
	defer c.Close()

	if t.starttls {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: t.host}); err != nil {
				return fmt.Errorf("mailer: starttls: %w", err)
			}
		} else if t.requireTLS {
			return fmt.Errorf("mailer: server %s does not offer STARTTLS but WithRequireTLS is set", t.addr)
		}
	}
	if t.auth != nil {
		if err := c.Auth(t.auth); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	if err := c.Mail(addressOnly(msg.From)); err != nil {
		return fmt.Errorf("mailer: MAIL FROM: %w", err)
	}
	for _, rcpt := range recipients(msg) {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("mailer: RCPT TO %s: %w", rcpt, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := wc.Write(raw); err != nil {
		return fmt.Errorf("mailer: write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("mailer: close body: %w", err)
	}
	return c.Quit()
}
