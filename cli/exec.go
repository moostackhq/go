package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// ExecOption configures one [Command.Exec] invocation. Options are
// passed as variadic arguments and applied in order, so later
// options override earlier ones.
type ExecOption func(*execConfig)

type execConfig struct {
	stdout    io.Writer
	stderr    io.Writer
	stdin     io.Reader
	parentCtx context.Context
	onError   func(Context, error) int
	env       func(string) (string, bool)
}

// WithStdout overrides the writer used for Context.Stdout(). Default
// is os.Stdout. Tests typically pass a bytes.Buffer.
func WithStdout(w io.Writer) ExecOption { return func(c *execConfig) { c.stdout = w } }

// WithStderr overrides the writer used for Context.Stderr() and for
// the default error renderer. Default is os.Stderr.
func WithStderr(w io.Writer) ExecOption { return func(c *execConfig) { c.stderr = w } }

// WithStdin overrides the reader used for Context.Stdin(). Default
// is os.Stdin.
func WithStdin(r io.Reader) ExecOption { return func(c *execConfig) { c.stdin = r } }

// WithSignalContext sets the parent context for the handler. By
// default Exec installs a signal-aware context that cancels on
// SIGINT and SIGTERM; supply this to opt out (pass
// context.Background()) or to use a custom-cancelling parent.
func WithSignalContext(ctx context.Context) ExecOption {
	return func(c *execConfig) { c.parentCtx = ctx }
}

// OnError supplies an error renderer + exit-code mapper. The default
// is [DefaultOnError], which honours the table documented on
// [Command.Exec].
func OnError(fn func(Context, error) int) ExecOption {
	return func(c *execConfig) { c.onError = fn }
}

// withEnv is internal: tests inject a fake environment.
func withEnv(env func(string) (string, bool)) ExecOption {
	return func(c *execConfig) { c.env = env }
}

// Exec parses args, runs the matched command's handler, and returns
// the process exit code. The conventional invocation in main is:
//
//	os.Exit(root.Exec(os.Args[1:]))
//
// For tests, pass explicit args and capture writers:
//
//	code := root.Exec(args, cli.WithStdout(&buf), cli.WithStderr(&buf))
//
// The default [Context] is cancelled on SIGINT / SIGTERM; handlers
// should select on <-ctx.Done() for long-running work.
//
// Default error → exit-code mapping (overridable via [OnError]):
//
//	nil                                 → 0
//	ctx.Err() == context.Canceled       → 130 (no output)
//	wraps ErrUsage / ErrValidation       → 2  (message + relevant help)
//	any other non-nil                   → 1  (message only)
func (c *Command) Exec(args []string, opts ...ExecOption) int {
	cfg := execConfig{
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		stdin:   os.Stdin,
		onError: DefaultOnError,
		env:     os.LookupEnv,
	}
	for _, o := range opts {
		o(&cfg)
	}

	parentCtx, cancel := cfg.signalContext()
	defer cancel()

	// Build a local snapshot of the user's Command tree before
	// injecting the built-in completion subcommands. This keeps
	// the user's *Command immutable across Exec calls, required
	// for safe concurrent invocation (tests, embedding) and to
	// avoid the surprise of a Subcommands slice that grows
	// behind the caller's back.
	root := withInjectedCompletion(c)

	res, known, parseErr := parseArgs(root, args, cfg.env)
	ctx := &cmdContext{
		Context:    parentCtx,
		stdout:     cfg.stdout,
		stderr:     cfg.stderr,
		stdin:      cfg.stdin,
		pathCmds:   res.path,
		knownFlags: known,
	}

	if parseErr != nil {
		return cfg.onError(ctx, parseErr)
	}

	if res.help {
		renderHelp(ctx.stdout, res.path, known)
		return 0
	}
	if res.version {
		fmt.Fprintln(ctx.stdout, res.cmd.Version)
		return 0
	}

	// Wire parsed values into the context so flag.Get(ctx) works.
	ctx.Context = ctxWithValues(ctx.Context, res.values)

	handler := composeHandler(res)
	if err := handler(ctx); err != nil {
		return cfg.onError(ctx, err)
	}
	return 0
}

// Parse runs the parser without executing any handler. The returned
// result lets tests inspect the resolved command and parsed values,
// or build their own custom dispatch.
//
// Returns an error wrapping [ErrUsage] (or another sentinel) for
// invalid invocations, in the same shape Exec would surface.
func (c *Command) Parse(args []string) (*parseResult, error) {
	res, _, err := parseArgs(c, args, os.LookupEnv)
	return res, err
}

// composeHandler wraps res.cmd.Run with every middleware declared
// on the path from root to cmd, in declaration order. Middleware
// from the root is outermost.
func composeHandler(res *parseResult) Handler {
	if res.cmd.Run == nil {
		return func(ctx Context) error {
			return fmt.Errorf("%s: no Run handler defined: %w", res.cmd.Name, ErrUsage)
		}
	}
	handler := Handler(res.cmd.Run)
	// Walk path inside-out so the root middleware is outermost.
	for i := len(res.path) - 1; i >= 0; i-- {
		ms := res.path[i].Use
		for j := len(ms) - 1; j >= 0; j-- {
			handler = ms[j](handler)
		}
	}
	return handler
}

func (cfg *execConfig) signalContext() (context.Context, context.CancelFunc) {
	if cfg.parentCtx != nil {
		return context.WithCancel(cfg.parentCtx)
	}
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// DefaultOnError implements the renderer + exit-code mapping
// documented on [Command.Exec]. Users that want a different output
// shape can supply their own via [OnError]; the default is exported
// so they can delegate to it for the cases they don't override.
func DefaultOnError(ctx Context, err error) int {
	if err == nil {
		return 0
	}
	prog := programName(ctx)
	stderr := ctx.Stderr()

	if errors.Is(err, context.Canceled) {
		return 130
	}

	// "Show me what's available": render help only, no message
	// line. Conventional UX for `myapp` (no subcommand) when
	// myapp is grouping-only.
	if errors.Is(err, ErrMissingSubcommand) {
		if cc, ok := ctx.(*cmdContext); ok && len(cc.pathCmds) > 0 {
			renderHelp(stderr, cc.pathCmds, cc.knownFlags)
		}
		return 2
	}

	switch {
	case errors.Is(err, ErrUsage), errors.Is(err, ErrValidation),
		errors.Is(err, ErrUnknownCmd), errors.Is(err, ErrMissingArg):
		fmt.Fprintf(stderr, "%s: %s\n", prog, displayMessage(err))
		if cc, ok := ctx.(*cmdContext); ok && len(cc.pathCmds) > 0 {
			fmt.Fprintln(stderr)
			renderHelp(stderr, cc.pathCmds, cc.knownFlags)
		}
		return 2
	default:
		fmt.Fprintf(stderr, "%s: %s\n", prog, displayMessage(err))
		return 1
	}
}

// displayMessage returns err.Error() with the trailing
// ": <sentinel>" suffix removed when present. The sentinel itself
// is still reachable via errors.Is for programmatic dispatch. The
// strip is purely a presentation tweak so the message line doesn't
// trail off into a parrot of the error category the user already
// inferred from context (or doesn't need).
func displayMessage(err error) string {
	msg := err.Error()
	for _, s := range []error{
		ErrUsage, ErrValidation, ErrUnknownCmd,
		ErrMissingArg, ErrMissingSubcommand,
	} {
		suffix := ": " + s.Error()
		if strings.HasSuffix(msg, suffix) {
			return msg[:len(msg)-len(suffix)]
		}
	}
	return msg
}

func programName(ctx Context) string {
	path := ctx.CommandPath()
	if len(path) == 0 {
		return "cli"
	}
	return path[0]
}
