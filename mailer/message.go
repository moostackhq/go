package mailer

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// render builds the full RFC 5322 message bytes for msg. The structure:
//
//   - text or html only → a single text/plain or text/html part
//   - text and html     → multipart/alternative
//   - any of the above + attachments → multipart/mixed wrapping the body
//     part followed by one part per attachment
//
// Text bodies use quoted-printable; attachments use base64. Bcc is
// deliberately omitted from the headers.
func render(msg *Message) ([]byte, error) {
	contentType, body, err := buildBody(msg)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	writeAddressHeader(&out, "From", []string{msg.From})
	writeAddressHeader(&out, "To", msg.To)
	writeAddressHeader(&out, "Cc", msg.Cc)
	if msg.ReplyTo != "" {
		writeAddressHeader(&out, "Reply-To", []string{msg.ReplyTo})
	}
	if msg.Subject != "" {
		writeHeader(&out, "Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	}
	writeHeader(&out, "Date", time.Now().Format(time.RFC1123Z))
	writeHeader(&out, "Message-ID", messageID(msg.From))
	for k, v := range msg.Headers {
		writeHeader(&out, k, v)
	}
	writeHeader(&out, "MIME-Version", "1.0")
	writeHeader(&out, "Content-Type", contentType)
	if !strings.HasPrefix(contentType, "multipart/") {
		writeHeader(&out, "Content-Transfer-Encoding", "quoted-printable")
	}
	out.WriteString("\r\n")
	out.Write(body)
	return out.Bytes(), nil
}

// buildBody returns the top-level Content-Type and the encoded body.
func buildBody(msg *Message) (contentType string, body []byte, err error) {
	if len(msg.Attachments) == 0 {
		return contentBody(msg)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	cbType, cbBody, err := contentBody(msg)
	if err != nil {
		return "", nil, err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", cbType)
	if !strings.HasPrefix(cbType, "multipart/") {
		h.Set("Content-Transfer-Encoding", "quoted-printable")
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", nil, err
	}
	if _, err := part.Write(cbBody); err != nil {
		return "", nil, err
	}

	for _, a := range msg.Attachments {
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		ah := textproto.MIMEHeader{}
		ah.Set("Content-Type", ct)
		ah.Set("Content-Transfer-Encoding", "base64")
		ah.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", a.Filename))
		ap, err := mw.CreatePart(ah)
		if err != nil {
			return "", nil, err
		}
		writeBase64(ap, a.Content)
	}
	if err := mw.Close(); err != nil {
		return "", nil, err
	}
	return `multipart/mixed; boundary="` + mw.Boundary() + `"`, buf.Bytes(), nil
}

// contentBody returns the message body excluding attachments: a single
// part, or multipart/alternative when both Text and HTML are present.
func contentBody(msg *Message) (contentType string, body []byte, err error) {
	hasText, hasHTML := msg.Text != "", msg.HTML != ""
	switch {
	case hasText && hasHTML:
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, p := range []struct{ ct, text string }{
			{"text/plain; charset=utf-8", msg.Text},
			{"text/html; charset=utf-8", msg.HTML},
		} {
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", p.ct)
			h.Set("Content-Transfer-Encoding", "quoted-printable")
			part, err := mw.CreatePart(h)
			if err != nil {
				return "", nil, err
			}
			if _, err := part.Write(quotedPrintable(p.text)); err != nil {
				return "", nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return "", nil, err
		}
		return `multipart/alternative; boundary="` + mw.Boundary() + `"`, buf.Bytes(), nil
	case hasHTML:
		return "text/html; charset=utf-8", quotedPrintable(msg.HTML), nil
	default:
		return "text/plain; charset=utf-8", quotedPrintable(msg.Text), nil
	}
}

func quotedPrintable(s string) []byte {
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

// writeBase64 writes data as base64 in 76-character lines, per MIME.
func writeBase64(w io.Writer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		_, _ = io.WriteString(w, enc[:76])
		_, _ = io.WriteString(w, "\r\n")
		enc = enc[76:]
	}
	_, _ = io.WriteString(w, enc)
	_, _ = io.WriteString(w, "\r\n")
}

func writeHeader(w io.Writer, key, value string) {
	_, _ = fmt.Fprintf(w, "%s: %s\r\n", key, value)
}

// writeAddressHeader formats addresses (RFC-encoding any display names)
// and writes the header, skipping an empty list.
func writeAddressHeader(w io.Writer, key string, addrs []string) {
	if len(addrs) == 0 {
		return
	}
	formatted := make([]string, len(addrs))
	for i, a := range addrs {
		formatted[i] = formatAddress(a)
	}
	writeHeader(w, key, strings.Join(formatted, ", "))
}

func formatAddress(s string) string {
	if a, err := mail.ParseAddress(s); err == nil {
		return a.String() // encodes a non-ASCII display name correctly
	}
	return s
}

func messageID(from string) string {
	domain := "localhost"
	if a, err := mail.ParseAddress(from); err == nil {
		if at := strings.LastIndex(a.Address, "@"); at >= 0 {
			domain = a.Address[at+1:]
		}
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("<%x@%s>", b, domain)
}
