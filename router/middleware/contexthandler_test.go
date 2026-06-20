package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func newCtxLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(ContextHandler(base)), &buf
}

func lastRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatalf("parse log line %q: %v", lines[len(lines)-1], err)
	}
	return m
}

func TestContextHandler_InjectsRequestID(t *testing.T) {
	log, buf := newCtxLogger(t)
	ctx := context.WithValue(context.Background(), requestIDKey, "abc123")

	log.InfoContext(ctx, "creating monitor")

	if got := lastRecord(t, buf)["request_id"]; got != "abc123" {
		t.Errorf("request_id = %v, want abc123", got)
	}
}

func TestContextHandler_NoIDWhenAbsent(t *testing.T) {
	log, buf := newCtxLogger(t)
	log.InfoContext(context.Background(), "startup")

	if _, ok := lastRecord(t, buf)["request_id"]; ok {
		t.Error("no request_id should be present outside a request")
	}
}

func TestContextHandler_Idempotent(t *testing.T) {
	log, buf := newCtxLogger(t)
	ctx := context.WithValue(context.Background(), requestIDKey, "abc123")

	// Caller already provides request_id (as Logger/Recover do) — must
	// not be duplicated.
	log.LogAttrs(ctx, slog.LevelInfo, "http request", slog.String("request_id", "explicit"))

	rec := lastRecord(t, buf)
	if rec["request_id"] != "explicit" {
		t.Errorf("explicit request_id should win, got %v", rec["request_id"])
	}
	if n := strings.Count(buf.String(), "request_id"); n != 1 {
		t.Errorf("request_id appears %d times, want 1 (no duplicate)", n)
	}
}

func TestContextHandler_PreservesWrappingThroughWith(t *testing.T) {
	log, buf := newCtxLogger(t)
	ctx := context.WithValue(context.Background(), requestIDKey, "abc123")

	// A logger derived via With must still inject request_id.
	log.With("component", "web").InfoContext(ctx, "msg")

	rec := lastRecord(t, buf)
	if rec["request_id"] != "abc123" {
		t.Errorf("derived logger lost context injection: %v", rec)
	}
	if rec["component"] != "web" {
		t.Errorf("With attr missing: %v", rec)
	}
}
