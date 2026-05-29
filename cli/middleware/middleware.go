// Package middleware ships the small set of cli.Middleware helpers
// the main package documents.
//
// Each helper is a plain function returning a cli.Middleware so
// composition with [cli.Command.Use] is just slice append.
package middleware

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/moostackhq/go/cli"
)

// Recover wraps the handler so a panic becomes an error rather than
// a crashed binary. The stack is written to ctx.Stderr.
func Recover() cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) (err error) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintln(ctx.Stderr(), string(debug.Stack()))
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return next(ctx)
		}
	}
}

// Timeout enforces an upper bound on handler execution. Passing 0
// disables it (the handler runs with the parent context unchanged).
// When the deadline elapses the handler's context is cancelled; the
// handler must select on <-ctx.Done() for the deadline to take
// effect on its own work.
func Timeout(d time.Duration) cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) error {
			if d <= 0 {
				return next(ctx)
			}
			withDeadline, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(wrapCtx(ctx, withDeadline))
		}
	}
}

// RequireEnv fails fast with a usage-style error if the named
// environment variable is unset or empty. Useful for early
// validation of operator-supplied secrets that the handler itself
// shouldn't have to range-check.
func RequireEnv(names ...string) cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) error {
			for _, n := range names {
				if os.Getenv(n) == "" {
					return cli.UsageError("required environment variable %s is not set", n)
				}
			}
			return next(ctx)
		}
	}
}

// Logging writes a one-line summary of the invocation to
// ctx.Stderr after the handler returns. Format:
//
//	<command path> took <duration> (err: <err>)
//
// Deliberately minimal; teams with real logging needs should use
// their own middleware that plugs into their logger.
func Logging() cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) error {
			start := time.Now()
			err := next(ctx)
			path := ""
			for i, p := range ctx.CommandPath() {
				if i > 0 {
					path += " "
				}
				path += p
			}
			if err != nil {
				fmt.Fprintf(ctx.Stderr(), "%s took %s (err: %v)\n", path, time.Since(start), err)
			} else {
				fmt.Fprintf(ctx.Stderr(), "%s took %s\n", path, time.Since(start))
			}
			return err
		}
	}
}

// wrapCtx returns a cli.Context that uses inner as its
// [context.Context] embedding but inherits the writers and command
// path from outer. This is how Timeout substitutes a
// shorter-deadline context underneath the handler without losing
// the cli.Context shape.
func wrapCtx(outer cli.Context, inner context.Context) cli.Context {
	return &derivedContext{Context: inner, parent: outer}
}

type derivedContext struct {
	context.Context
	parent cli.Context
}

func (d *derivedContext) Stdout() io.Writer    { return d.parent.Stdout() }
func (d *derivedContext) Stderr() io.Writer    { return d.parent.Stderr() }
func (d *derivedContext) Stdin() io.Reader     { return d.parent.Stdin() }
func (d *derivedContext) CommandPath() []string { return d.parent.CommandPath() }
