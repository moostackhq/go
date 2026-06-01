package jobs

import (
	"context"
	"log/slog"
)

// Context extends [context.Context] with job-aware accessors. The
// runtime constructs a Context per attempt and passes it to
// [Job.Run]; the same value is reachable inside [Step] via the
// embedded stdlib context.
type Context interface {
	context.Context
	JobID() string
	Kind() string
	Attempt() int
	Logger() *slog.Logger
	Progress(done, total int64, msg string)
}

// jobCtx is the concrete [Context] the runner builds for each
// attempt. It embeds the stdlib context the runner derives (so
// Deadline / Done / Err / Value all work without forwarding), and
// stores the per-job metadata directly.
//
// The pointer to jobState is the durable channel back into the
// runtime: progress reports and step bookkeeping write to it; the
// runner reads it back when Run returns.
type jobCtx struct {
	context.Context
	state *jobState
}

func (c *jobCtx) JobID() string        { return c.state.jobID }
func (c *jobCtx) Kind() string         { return c.state.kind }
func (c *jobCtx) Attempt() int         { return c.state.attempt }
func (c *jobCtx) Logger() *slog.Logger { return c.state.logger }

func (c *jobCtx) Progress(done, total int64, msg string) {
	// Capture the latest reported progress on jobState. A 500ms
	// throttle goroutine reads it back and writes to the store; the
	// runner also flushes the final value as part of Outcome.
	c.state.mu.Lock()
	c.state.lastProgress = Progress{Done: done, Total: total, Msg: msg}
	c.state.progressDirty = true
	c.state.mu.Unlock()
}

// Value also returns the jobState when the requested key is the
// internal ctxKey, so [Step] can reach the state via a plain
// context.Context without a type assertion to jobCtx.
func (c *jobCtx) Value(key any) any {
	if _, ok := key.(ctxKey); ok {
		return c.state
	}
	return c.Context.Value(key)
}
