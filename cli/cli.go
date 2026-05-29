// Package cli is an opinionated, typed CLI library for Go.
//
// Flags and positional arguments are declared as typed values; the
// variable IS the accessor. Renaming a flag is a Go refactor the
// compiler completes for you.
//
//	var port = cli.IntFlag("port").Default(8080).Env("PORT")
//
//	root := &cli.Command{
//	    Name:  "myapp",
//	    Flags: cli.Flags(port),
//	    Run: func(ctx cli.Context) error {
//	        listen(port.Get(ctx))
//	        return nil
//	    },
//	}
//
//	os.Exit(root.Exec(os.Args[1:]))
//
// There is one type for the top-level binary and every subcommand:
// [Command]. Concerns that only make sense at the top level (writers,
// signal context, error rendering) are configured via [ExecOption]
// arguments to [Command.Exec], not as fields on [Command].
package cli

import (
	"context"
	"errors"
	"io"
)

var (
	// ErrUsage is wrapped by parse-time errors that the user can
	// fix by changing their invocation (unknown flag, missing
	// required value, validation failure with a fixable cause).
	// The default error renderer appends the relevant command's
	// help when this is the cause.
	ErrUsage = errors.New("usage error")

	// ErrValidation is wrapped when a [Flag] or [Arg] validator
	// rejects a parsed value.
	ErrValidation = errors.New("validation failed")

	// ErrUnknownCmd is wrapped when the user invokes a subcommand
	// the tree does not contain.
	ErrUnknownCmd = errors.New("unknown command")

	// ErrMissingArg is wrapped when a required positional argument
	// was not supplied.
	ErrMissingArg = errors.New("missing argument")

	// ErrMissingSubcommand is returned when the user invokes a
	// grouping-only command (Subcommands set, Run nil) without
	// supplying a subcommand. The default renderer treats this
	// as a "show me what's available" signal: it prints the
	// command's help to stderr and exits with the usage-error
	// code, without printing a separate error message line.
	ErrMissingSubcommand = errors.New("missing subcommand")
)

// Command is the unit. The top-level binary is a Command; every
// subcommand is a Command. There is no separate "app" wrapper type.
type Command struct {
	// Name is the token used to invoke the command. For the root
	// it is the program name (used in help output and error
	// prefixes); for subcommands it is what the user types after
	// the parent.
	Name string

	// Help is the one-line summary shown in parent help listings
	// and at the top of the command's own --help.
	Help string

	// Long is the optional multi-line description shown below the
	// usage line in --help.
	Long string

	// Version is the string emitted by --version. Conventionally
	// set on the root only.
	Version string

	// Flags declared on this command. Flags on a parent are
	// inherited by children; call Get(ctx) on the flag variable
	// from any descendant handler.
	Flags []AnyFlag

	// Args are the positional arguments, in binding order. At most
	// one variadic, and only as the last entry. Optional args must
	// follow all required args.
	//
	// A token that starts with "-" is normally interpreted as a
	// flag. To pass such a token as a positional (e.g. "-1" as a
	// value) the user must put "--" before it: `myapp cmd -- -1`.
	// Tokens after a variadic positional are likewise treated as
	// positionals regardless of leading dash.
	Args []AnyArg

	// Subcommands of this command, dispatched by Name. A Command
	// can have both Subcommands and Run: Run handles the bare
	// invocation, subcommands are dispatched when the user supplies
	// one.
	Subcommands []*Command

	// Use is a stack of middleware applied in declaration order:
	// Use[0] is the outermost wrapper. Middleware declared on a
	// parent applies to every descendant.
	Use []Middleware

	// Run is invoked after parsing succeeds. May be nil for
	// grouping-only commands; in that case invoking the command
	// without a subcommand prints help and exits with ErrUsage.
	Run func(Context) error

	// Examples shown in --help under an "Examples:" section.
	Examples []Example

	// Hidden omits the command from parent listings.
	Hidden bool
}

// Example is a single entry in a command's --help examples section.
type Example struct {
	// Cmd is the example invocation line.
	Cmd string
	// Help is a one-line description rendered next to Cmd.
	Help string
}

// Context is what [Command.Run] receives. It embeds [context.Context]
// for cancellation and adds the request-scoped accessors handlers
// need.
type Context interface {
	context.Context

	// Stdout is the writer set by [WithStdout] or, by default, the
	// process's os.Stdout.
	Stdout() io.Writer

	// Stderr is the writer set by [WithStderr] or, by default, the
	// process's os.Stderr.
	Stderr() io.Writer

	// Stdin is the reader set by [WithStdin] or, by default,
	// os.Stdin.
	Stdin() io.Reader

	// CommandPath is the slice of names from root to the command
	// currently being executed, e.g. {"myapp", "db", "migrate"}.
	CommandPath() []string
}

// Handler is the shape of a runnable command body.
type Handler func(Context) error

// Middleware wraps a Handler with additional behaviour. Composition
// follows net/http semantics: declaration order is outer-to-inner.
type Middleware func(Handler) Handler

// AnyFlag is the unexported-method interface that every *Flag[T]
// satisfies. The methods exist so the parser can talk to flags
// uniformly without caring about T; users construct flags via the
// typed constructors ([String], [Int], etc.) and never write an
// implementation themselves.
type AnyFlag interface {
	flagName() string
	flagShort() rune
	flagHelp() string
	flagEnv() string
	flagRequired() bool
	flagHidden() bool
	flagGroup() string
	flagIsBool() bool
	flagDefaultText() string
	flagPlaceholder() string
	flagApplyString(values, string) error
	flagApplyEnv(values, func(string) (string, bool)) error
	flagValidate(values) error
	flagPresent(values) bool
	flagHasDefault() bool
	flagCompleter() CompleteFn
}

// AnyArg is the unexported-method interface that every *Arg[T]
// satisfies. Same rationale as [AnyFlag].
type AnyArg interface {
	argName() string
	argHelp() string
	argRequired() bool
	argVariadic() bool
	argIsSlice() bool
	argApplyString(values, string) error
	argPresent(values) bool
	argValidate(values) error
	argCompleter() CompleteFn
}

// Flags is a convenience constructor for the []AnyFlag field on
// [Command]. It exists so callers can pass typed *Flag[T] values
// directly without spelling out the slice literal each time.
func Flags(flags ...AnyFlag) []AnyFlag {
	return flags
}

// Args is the positional equivalent of [Flags].
func Args(args ...AnyArg) []AnyArg {
	return args
}

// values is the per-invocation map of parsed flag/arg values. Keyed
// by the *Flag[T] / *Arg[T] pointer so lookups are typed and never
// collide. Carried on the Context via a private key so the same Flag
// variable is safe across concurrent Exec calls.
type values map[any]any

type valuesKey struct{}

func valuesFromCtx(ctx context.Context) values {
	v, _ := ctx.Value(valuesKey{}).(values)
	if v == nil {
		// A handler invoked outside Exec: return an empty map so
		// Get returns zero values rather than panicking on a nil
		// map read.
		return values{}
	}
	return v
}

func ctxWithValues(parent context.Context, v values) context.Context {
	return context.WithValue(parent, valuesKey{}, v)
}

// cmdContext is the concrete implementation of Context. It carries
// the resolved command path and the flag set visible at that command
// so the default error renderer can append the right help section
// without re-walking the tree.
type cmdContext struct {
	context.Context
	stdout     io.Writer
	stderr     io.Writer
	stdin      io.Reader
	pathCmds   []*Command
	knownFlags []AnyFlag
}

func (c *cmdContext) Stdout() io.Writer { return c.stdout }
func (c *cmdContext) Stderr() io.Writer { return c.stderr }
func (c *cmdContext) Stdin() io.Reader  { return c.stdin }
func (c *cmdContext) CommandPath() []string {
	out := make([]string, len(c.pathCmds))
	for i, p := range c.pathCmds {
		out[i] = p.Name
	}
	return out
}

// CompleteContext is passed to a [CompleteFn] when the runtime
// completion subcommand invokes it.
//
// Word is the current partial token the shell is asking the
// library to complete, typically the value the user has typed so
// far up to the cursor. A [CompleteFn] should return candidates
// that begin with Word; the runtime filters out non-matching
// entries before emitting to the shell, but pre-filtering keeps
// dynamic candidate lists small.
//
// Args is every token the shell sent, in order, with Word as the
// last entry. Useful when the right candidates depend on values
// supplied earlier on the line (e.g. "complete a column name
// against the table the user already named").
type CompleteContext struct {
	Word string
	Args []string
}

// CompleteFn returns the candidate strings for the current token.
// The shell-completion runtime invokes it; flags and args attach one
// via .Complete().
type CompleteFn func(CompleteContext) []string

// StaticValues returns a [CompleteFn] that always suggests the
// same fixed candidates. Used internally by [Flag.OneOf] and
// [Arg.OneOf]; exposed so callers can register a static list
// directly via .Complete() without going through OneOf's
// validator side effect.
func StaticValues(values ...string) CompleteFn {
	cp := make([]string, len(values))
	copy(cp, values)
	return func(CompleteContext) []string { return cp }
}

