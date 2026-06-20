package mailer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
)

// captureTransport records the message it's handed.
type captureTransport struct{ msg *Message }

func (c *captureTransport) Send(_ context.Context, msg *Message) error {
	c.msg = msg
	return nil
}

func parse(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v\n%s", err, raw)
	}
	return m
}

func TestRender_TextOnly(t *testing.T) {
	raw, err := render(&Message{From: "a@x.com", To: []string{"b@y.com"}, Subject: "Hi", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	if ct := m.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if got := m.Header.Get("From"); got != "<a@x.com>" {
		t.Errorf("From = %q", got)
	}
	body, _ := io.ReadAll(m.Body)
	if !strings.Contains(string(body), "hello") {
		t.Errorf("body missing text: %q", body)
	}
}

func TestRender_HTMLOnly(t *testing.T) {
	raw, err := render(&Message{From: "a@x.com", To: []string{"b@y.com"}, Subject: "Hi", HTML: "<b>hi</b>"})
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	if ct := m.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestNewSMTP_InvalidAddress(t *testing.T) {
	if _, err := NewSMTP("missing-port"); err == nil {
		t.Error("NewSMTP without host:port should error")
	}
}

func TestSMTPTransport_RequireTLSFailsWithoutSTARTTLS(t *testing.T) {
	addr, _ := fakeSMTP(t) // the fake server doesn't advertise STARTTLS
	tr, err := NewSMTP(addr, WithRequireTLS())
	if err != nil {
		t.Fatal(err)
	}
	m := New(tr, WithDefaultFrom("a@x.com"))
	err = m.Send(context.Background(), &Message{To: []string{"b@y.com"}, Subject: "Hi", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("expected a STARTTLS-required error, got %v", err)
	}
}

func TestRender_Alternative(t *testing.T) {
	raw, err := render(&Message{
		From: "a@x.com", To: []string{"b@y.com"}, Subject: "Hi",
		Text: "plain version", HTML: "<p>html version</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	mt, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mt)
	}
	types := partTypes(t, m.Body, params["boundary"])
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Errorf("alternative parts = %v, want [text/plain, text/html]", types)
	}
}

func TestRender_WithAttachment(t *testing.T) {
	raw, err := render(&Message{
		From: "a@x.com", To: []string{"b@y.com"}, Subject: "Report", Text: "see attached",
		Attachments: []Attachment{{Filename: "r.txt", Content: []byte("data")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	mt, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("Content-Type = %q, want multipart/mixed", mt)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	body, err := mr.NextPart() // first part: the body
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("first part = %q, want text/plain", body.Header.Get("Content-Type"))
	}
	att, err := mr.NextPart() // second part: the attachment
	if err != nil {
		t.Fatal(err)
	}
	if cd := att.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="r.txt"`) {
		t.Errorf("attachment disposition = %q", cd)
	}
	if att.Header.Get("Content-Transfer-Encoding") != "base64" {
		t.Errorf("attachment not base64-encoded")
	}
}

func TestRender_BccNotInHeaders(t *testing.T) {
	raw, err := render(&Message{
		From: "a@x.com", To: []string{"b@y.com"}, Bcc: []string{"secret@z.com"},
		Subject: "Hi", Text: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	if m.Header.Get("Bcc") != "" {
		t.Error("Bcc must not appear in headers")
	}
	header := string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))])
	if strings.Contains(header, "secret@z.com") {
		t.Errorf("bcc address leaked into headers:\n%s", header)
	}
}

func partTypes(t *testing.T, r io.Reader, boundary string) []string {
	t.Helper()
	mr := multipart.NewReader(r, boundary)
	var types []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, p.Header.Get("Content-Type"))
	}
	return types
}

func TestSend_Validation(t *testing.T) {
	m := New(&captureTransport{})
	ctx := context.Background()

	tests := []struct {
		name string
		msg  *Message
		want error
	}{
		{"no sender", &Message{To: []string{"b@y.com"}, Text: "x"}, ErrNoSender},
		{"no recipients", &Message{From: "a@x.com", Text: "x"}, ErrNoRecipients},
		{"no body", &Message{From: "a@x.com", To: []string{"b@y.com"}}, ErrNoBody},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.Send(ctx, tc.msg); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("invalid address", func(t *testing.T) {
		err := m.Send(ctx, &Message{From: "a@x.com", To: []string{"not an address"}, Text: "x"})
		if err == nil {
			t.Error("expected an invalid-address error")
		}
	})
}

func TestSend_RejectsHeaderInjection(t *testing.T) {
	m := New(&captureTransport{}, WithDefaultFrom("a@x.com"))
	ctx := context.Background()
	base := func() *Message {
		return &Message{To: []string{"b@y.com"}, Subject: "Hi", Text: "x"}
	}

	cases := map[string]map[string]string{
		"CRLF in value": {"X-Custom": "ok\r\nBcc: attacker@evil.com"},
		"LF in value":   {"X-Custom": "ok\nX-Injected: yes"},
		"NUL in value":  {"X-Custom": "ok\x00bad"},
		"CRLF in key":   {"X-Bad\r\nBcc": "v"},
		"colon in key":  {"X:Bad": "v"},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			msg := base()
			msg.Headers = h
			if err := m.Send(ctx, msg); !errors.Is(err, ErrBadHeader) {
				t.Errorf("err = %v, want ErrBadHeader", err)
			}
		})
	}
}

func TestSend_RejectsMalformedHeaderKey(t *testing.T) {
	m := New(&captureTransport{}, WithDefaultFrom("a@x.com"))
	// Leading/trailing/embedded space, empty, and tab are not valid field
	// names — a leading-space key would otherwise fold into the prior header.
	for _, k := range []string{" X-Foo", "X-Foo ", "X Foo", "", "X-Foo\tBar"} {
		t.Run(fmt.Sprintf("%q", k), func(t *testing.T) {
			err := m.Send(context.Background(), &Message{
				To: []string{"b@y.com"}, Subject: "Hi", Text: "x",
				Headers: map[string]string{k: "v"},
			})
			if !errors.Is(err, ErrBadHeader) {
				t.Errorf("key %q: err = %v, want ErrBadHeader", k, err)
			}
		})
	}
}

func TestSend_RejectsReservedHeaderOverride(t *testing.T) {
	m := New(&captureTransport{}, WithDefaultFrom("a@x.com"))
	for _, name := range []string{"Content-Type", "content-type", "MIME-Version", "From", "Subject"} {
		t.Run(name, func(t *testing.T) {
			err := m.Send(context.Background(), &Message{
				To: []string{"b@y.com"}, Subject: "Hi", Text: "x",
				Headers: map[string]string{name: "evil"},
			})
			if !errors.Is(err, ErrBadHeader) {
				t.Errorf("override of %q: err = %v, want ErrBadHeader", name, err)
			}
		})
	}
}

func TestSend_AllowsCleanCustomHeader(t *testing.T) {
	cap := &captureTransport{}
	m := New(cap, WithDefaultFrom("a@x.com"))
	err := m.Send(context.Background(), &Message{
		To: []string{"b@y.com"}, Subject: "Hi", Text: "x",
		Headers: map[string]string{"List-Unsubscribe": "<https://x.test/u>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := render(cap.msg)
	if !strings.Contains(string(raw), "List-Unsubscribe: <https://x.test/u>") {
		t.Errorf("clean custom header missing:\n%s", raw)
	}
}

func TestRender_EmptySubjectOmitted(t *testing.T) {
	raw, err := render(&Message{From: "a@x.com", To: []string{"b@y.com"}, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if parse(t, raw).Header.Get("Subject") != "" {
		t.Error("empty Subject should be omitted, not emitted blank")
	}
}

func TestSend_DefaultFromAppliedWithoutMutatingCaller(t *testing.T) {
	cap := &captureTransport{}
	m := New(cap, WithDefaultFrom("alerts@example.com"))

	orig := &Message{To: []string{"b@y.com"}, Text: "x"}
	if err := m.Send(context.Background(), orig); err != nil {
		t.Fatal(err)
	}
	if cap.msg.From != "alerts@example.com" {
		t.Errorf("default From not applied: %q", cap.msg.From)
	}
	if orig.From != "" {
		t.Error("Send mutated the caller's message")
	}
}

func TestLogTransport_Logs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	m := New(NewLogTransport(logger), WithDefaultFrom("a@x.com"))

	if err := m.Send(context.Background(), &Message{To: []string{"b@y.com"}, Subject: "Down", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Down") {
		t.Errorf("log transport didn't record the subject:\n%s", buf.String())
	}
}

// fakeSMTP starts a minimal in-process SMTP server and returns its
// address plus an accessor for the bytes received in the DATA phase.
func fakeSMTP(t *testing.T) (addr string, data func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var received strings.Builder

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake ESMTP\r\n")
		inData := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					fmt.Fprint(conn, "250 OK\r\n")
					continue
				}
				mu.Lock()
				received.WriteString(line)
				mu.Unlock()
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				fmt.Fprint(conn, "250-fake\r\n250 OK\r\n") // no STARTTLS advertised
			case cmd == "DATA":
				fmt.Fprint(conn, "354 end with .\r\n")
				inData = true
			case cmd == "QUIT":
				fmt.Fprint(conn, "221 bye\r\n")
				return
			default: // MAIL FROM, RCPT TO, etc.
				fmt.Fprint(conn, "250 OK\r\n")
			}
		}
	}()

	return ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return received.String()
	}
}

func TestSMTPTransport_SendsRenderedMessage(t *testing.T) {
	addr, data := fakeSMTP(t)
	tr, err := NewSMTP(addr)
	if err != nil {
		t.Fatal(err)
	}
	m := New(tr, WithDefaultFrom("alerts@example.com"))

	err = m.Send(context.Background(), &Message{
		To:      []string{"ops@example.com"},
		Subject: "prod is DOWN",
		Text:    "monitor prod failed",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	body := data()
	for _, want := range []string{"Subject: prod is DOWN", "monitor prod failed", "To: <ops@example.com>"} {
		if !strings.Contains(body, want) {
			t.Errorf("DATA missing %q\n--- received ---\n%s", want, body)
		}
	}
}
