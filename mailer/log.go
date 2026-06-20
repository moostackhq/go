package mailer

import (
	"context"
	"log/slog"
	"strings"
)

// logTransport "delivers" by logging the message instead of sending it —
// the development backend, so you can see what would go out without an
// SMTP server. Like an in-memory store: zero infrastructure.
type logTransport struct {
	logger *slog.Logger
}

// NewLogTransport returns a [Transport] that logs each message at Info
// (and its rendered body at Debug) rather than sending it. A nil logger
// uses [slog.Default].
func NewLogTransport(logger *slog.Logger) Transport {
	if logger == nil {
		logger = slog.Default()
	}
	return &logTransport{logger: logger}
}

func (t *logTransport) Send(ctx context.Context, msg *Message) error {
	t.logger.InfoContext(ctx, "mailer: email not sent (log transport)",
		"from", msg.From,
		"to", strings.Join(msg.To, ", "),
		"subject", msg.Subject,
	)
	if t.logger.Enabled(ctx, slog.LevelDebug) {
		if raw, err := render(msg); err == nil {
			t.logger.DebugContext(ctx, "mailer: email body", "rfc822", string(raw))
		}
	}
	return nil
}
