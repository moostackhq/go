package csrf

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"testing"
)

func testSecret() []byte { return bytes.Repeat([]byte("k"), 32) }

func TestNew_RejectsShortSecret(t *testing.T) {
	if _, err := New(Config{Secret: []byte("too short")}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if _, err := New(Config{Secret: testSecret()}); err != nil {
		t.Fatalf("32-byte secret should be valid, got %v", err)
	}
}

func TestNew_Defaults(t *testing.T) {
	p, _ := New(Config{Secret: testSecret()})
	if p.cookie.Name != defaultCookieName || p.fieldName != defaultFieldName || p.headerName != defaultHeaderName {
		t.Errorf("defaults not applied: cookie=%q field=%q header=%q", p.cookie.Name, p.fieldName, p.headerName)
	}
	if p.cookie.Path != "/" {
		t.Errorf("cookie path = %q, want /", p.cookie.Path)
	}
}

func TestSignCookie_RoundTripAndTamper(t *testing.T) {
	p, _ := New(Config{Secret: testSecret()})
	tok, _ := newToken()

	v := p.signCookie(tok)
	got, ok := p.parseCookie(v)
	if !ok || !bytes.Equal(got, tok) {
		t.Fatalf("round-trip failed: ok=%v equal=%v", ok, bytes.Equal(got, tok))
	}

	// A different secret must reject the cookie (forged/wrong signer).
	other, _ := New(Config{Secret: bytes.Repeat([]byte("x"), 32)})
	if _, ok := other.parseCookie(v); ok {
		t.Error("cookie verified under a different secret")
	}
	// Garbage and empty are rejected.
	if _, ok := p.parseCookie("not-base64!!"); ok {
		t.Error("garbage cookie accepted")
	}
	if _, ok := p.parseCookie(""); ok {
		t.Error("empty cookie accepted")
	}
}

func TestMaskUnmask_RoundTripAndUnique(t *testing.T) {
	tok, _ := newToken()
	a, _ := maskToken(tok)
	b, _ := maskToken(tok)
	if a == b {
		t.Error("two maskings produced the same value (no per-render randomness)")
	}
	for _, m := range []string{a, b} {
		got, ok := unmaskToken(m)
		if !ok || !bytes.Equal(got, tok) {
			t.Errorf("unmask round-trip failed for %q", m)
		}
	}
	if _, ok := unmaskToken("short"); ok {
		t.Error("malformed masked token accepted")
	}
}

func TestField_EmptyWithoutMiddleware(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := Field(r); got != "" {
		t.Errorf("Field without middleware = %q, want empty", got)
	}
	if got := Token(r); got != "" {
		t.Errorf("Token without middleware = %q, want empty", got)
	}
}
