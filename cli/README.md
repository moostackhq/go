# cli

An opinionated, typed CLI library for Go.

## Features

| Feature | What it gives you |
|---|---|
| Typed flag access | `var port = cli.IntFlag("port")` then `port.Get(ctx)` returns `int`. No string keys, no reflection, no type assertions. |
| One `Command` type | The top-level binary and every subcommand share the same struct. No separate App / wrapper / runtime context to learn. |
| Inherited flags | A flag declared on a parent is visible from every descendant handler via the same `Get(ctx)`. |
| Validators declared with the flag | `.Required()`, `.OneOf(...)`, `.Validate(fn)` — all run before the handler. |
| Repeatable + variadic | `cli.StringSliceFlag` for repeatable flags, `cli.StringSliceArg(name).Variadic()` for catch-all positionals. |
| Middlewares | `Use: []cli.Middleware{...}` — outer-to-inner composition, inherited from parent commands. |
| Free shell completion | Auto-injected `completion bash\|zsh\|fish` emits a script that calls the binary back for runtime candidates. |
| Auto-generated help | `--help` derived from typed definitions: usage line, args, flag groups, defaults, env/required annotations, term-width-aware. |
| Stdlib shapes | `context.Context`, `errors.Is`, `func(Handler) Handler`. No new idioms to memorise. |
| Testable without spawning a process | `root.Exec(args, cli.WithStdout(buf), cli.WithStderr(buf))`. No `os.Args`, no `os.Exit`, no `os.Stdout`. |

## Install

```bash
go get github.com/moostackhq/go/cli
```

Stdlib + `golang.org/x/term` only. No third-party deps in the core.

## Quickstart

```go
package main

import (
	"fmt"
	"os"

	"github.com/moostackhq/go/cli"
)

// Flags are package-level typed values. The variable is the accessor.
var (
	port    = cli.IntFlag("port").Default(8080).Env("PORT").Help("port to listen on")
	verbose = cli.BoolFlag("verbose").Short('v').Help("chatty logs")
	name    = cli.StringArg("name").Required().Help("server name")
)

func main() {
	root := &cli.Command{
		Name:  "greet",
		Help:  "say hello over HTTP",
		Flags: cli.Flags(port, verbose),
		Args:  cli.Args(name),
		Run: func(ctx cli.Context) error {
			fmt.Fprintf(ctx.Stdout(), "hello, %s — listening on :%d (verbose=%v)\n",
				name.Get(ctx), port.Get(ctx), verbose.Get(ctx))
			return nil
		},
	}
	os.Exit(root.Exec(os.Args[1:]))
}
```

```
$ greet --port 9090 alice
hello, alice — listening on :9090 (verbose=false)

$ greet --help
Usage:
  greet [flags] <name>
...
```

A larger example with subcommands, validators, JSON persistence, and middleware lives in [`example/todo`](./example/todo/main.go).

## Flags

### Constructors

```go
cli.StringFlag(name)        // *Flag[string]
cli.IntFlag(name)           // *Flag[int]
cli.Int64Flag(name)         // *Flag[int64]
cli.FloatFlag(name)         // *Flag[float64]
cli.BoolFlag(name)          // *Flag[bool]
cli.DurationFlag(name)      // *Flag[time.Duration]
cli.TimeFlag(name)          // *Flag[time.Time]  (RFC3339)
cli.StringSliceFlag(name)   // *Flag[[]string]   (repeatable)
cli.IntSliceFlag(name)      // *Flag[[]int]
cli.CustomFlag[T](name, parse)  // generic escape hatch
```

Constructor names match their `Arg` siblings (`cli.IntFlag` ↔ `cli.IntArg`) so flags and positionals stay visually paired across the API.

### Builder methods

All return `*Flag[T]` for chaining.

| Method | Purpose |
|---|---|
| `.Default(v)` | Value `Get` returns when the flag is unset. |
| `.Env(name)` | Bind to an env var (used when the flag is not on the command line). |
| `.Short(r)` | Single-character alias (`-v`). |
| `.Help(s)` | One-line description shown in `--help`. |
| `.Required()` | Parse fails if the flag is unset and no default/env value resolves. |
| `.Validate(fn)` | Custom validator; runs after parse. |
| `.OneOf(values...)` | Restrict accepted values; also feeds shell completion. |
| `.Placeholder(s)` | Metavariable in help (`--port <N>`). Defaults to upper-cased name. |
| `.Group(name)` | Categorise the flag under a header in help. |
| `.Hidden()` | Omit from help; still parseable. |
| `.Complete(fn)` | Shell-completion strategy. |

### Accessors

```go
v := port.Get(ctx)            // typed, compile-checked
v, set := port.Lookup(ctx)    // distinguishes explicit from default
```

## Args

Positional arguments use the same builder shape.

```go
cli.StringArg(name)      // *Arg[string]
cli.IntArg(name)         // *Arg[int]
cli.StringSliceArg(name) // *Arg[[]string]  (combine with .Variadic())
cli.CustomArg[T](name, parse)
```

| Builder | Purpose |
|---|---|
| `.Required()` | Parse fails if the arg is missing. |
| `.Default(v)` | Used when not supplied (only valid on optional args). |
| `.Help(s)` | Description in help. |
| `.Validate(fn)` | Custom validator. |
| `.OneOf(values...)` | Restrict accepted values + completion. |
| `.Variadic()` | Slice arg captures every remaining token. Must be last. |
| `.Complete(fn)` | Shell-completion strategy. |

Rules enforced at declaration time:
- At most one variadic, and only as the last arg.
- Required args must precede optional args.
- Slice args must be `.Variadic()`.

A token starting with `-` is normally a flag. To pass `-1` (or similar) as a positional value, use `--`: `myapp cmd -- -1`. Tokens after a variadic positional are likewise positional regardless of leading dash.

## Commands

```go
type Command struct {
    Name        string         // invocation token (and program name on the root)
    Help        string         // one-line summary
    Long        string         // optional long description
    Version     string         // shown on --version (usually root only)
    Flags       []AnyFlag
    Args        []AnyArg
    Subcommands []*Command
    Use         []Middleware   // applied outer-to-inner; inherited by children
    Run         func(Context) error  // nil for grouping-only commands
    Examples    []Example
    Hidden      bool
}
```

Top-level execution:

```go
os.Exit(root.Exec(os.Args[1:]))
```

There is no separate `App` type. The root is just another `Command`. I/O writers, signal context, and error rendering — concerns that only make sense at the top level — are configured via `ExecOption` arguments, not as fields on the struct.

### Subcommands

Plain `*Command` values nested under `Subcommands`. Subcommands inherit parent flags and middleware automatically.

```go
db := &cli.Command{
    Name:        "db",
    Help:        "database utilities",
    Flags:       cli.Flags(dsn),                 // visible to migrate, status, ...
    Subcommands: []*cli.Command{migrateCmd, statusCmd},
}

root := &cli.Command{
    Name:        "myapp",
    Subcommands: []*cli.Command{serveCmd, db},
}
```

Routing:
- Tokens are matched against subcommand names left-to-right; the deepest match wins.
- Flags can appear anywhere (before or after the subcommand) for normal commands. A command with a variadic positional locks flag parsing after the first positional so the variadic catches every remaining token unchanged.
- Bare invocation of a grouping-only command (`Run` nil, `Subcommands` set) renders help and exits 2 — no error message above it.
- Unknown subcommand names suggest the closest match via Levenshtein distance.

### Middleware

```go
type Handler func(Context) error
type Middleware func(Handler) Handler
```

Declaration order is outer-to-inner: `Use[0]` is the outermost wrapper. Middleware on a parent command applies to every descendant.

The library ships a small set in [`./middleware`](./middleware): `Recover`, `Timeout`, `RequireEnv`, `Logging`.

## Help, errors, completion

All three are derived from the same typed definitions you already write; you don't author help text or completion scripts separately.

### Help

`--help` and `-h` are reserved on every command. The renderer:
- Detects terminal width via `golang.org/x/term` and wraps long descriptions; falls back to 80 cols when output is not a TTY.
- Bold section headers (`Usage:`, `Flags:`, etc.) when stderr/stdout is a TTY and `NO_COLOR` is unset.
- Annotates flags with `(required)`, `(default: X)`, `(env: VAR)` derived from the builder calls.
- Groups flags by `.Group()` if set.

### Errors

Errors are values. The library exports sentinels callers can match with `errors.Is`:

```go
ErrUsage              // bad flags / args
ErrValidation         // .Validate() / .OneOf() rejected a value
ErrUnknownCmd         // typo'd subcommand
ErrMissingArg         // required positional missing
ErrMissingSubcommand  // grouping-only command invoked bare
```

Handler code can wrap with `cli.UsageError(format, args...)` — the default renderer appends the relevant command's help below the error line.

| Error class | Stderr output | Exit |
|---|---|---|
| `nil` | — | 0 |
| `ErrMissingSubcommand` | help only, no message line | 2 |
| `ErrUsage` / `ErrValidation` / `ErrUnknownCmd` / `ErrMissingArg` | `prog: <message>` + relevant help | 2 |
| `context.Canceled` | nothing | 130 |
| any other | `prog: <message>` | 1 |

Override by passing `cli.OnError(fn)` to `Exec`. The default lives at `cli.DefaultOnError` if you want to delegate the cases you don't override.

### Shell completion

The library auto-injects two hidden subcommands on every Exec:

```
$ myapp completion bash > /etc/bash_completion.d/myapp
$ myapp completion zsh  > "${fpath[1]}/_myapp"
$ myapp completion fish > ~/.config/fish/completions/myapp.fish
```

The generated scripts call back into the binary via a hidden `__complete` subcommand, which walks the command tree against the partial token list and emits candidates:

- Subcommand names at the resolved command level
- Long flag names after `--<prefix>`
- Short + long flag names after a bare `-`
- A flag's value via its `Complete(fn)` (or `OneOf(values...)`) for both `--mode <TAB>` and `--mode=<TAB>` forms
- Bool flags as `--verbose=true` / `--verbose=false` for the inline form
- The next positional's `Complete(fn)`, or the trailing variadic's once positionals are exhausted

Built-in candidate sources:

```go
cli.StaticValues(values...)  // fixed list
```

`OneOf` automatically registers a `StaticValues` completer alongside its validator.

## Configuration

```go
type ExecOption func(*execConfig)

cli.WithStdout(w io.Writer)
cli.WithStderr(w io.Writer)
cli.WithStdin(r io.Reader)
cli.WithSignalContext(ctx context.Context)   // default: SIGINT + SIGTERM aware
cli.OnError(func(Context, error) int)        // override the default renderer
```

`Exec` snapshots the command tree before injecting the completion subcommands, so the user's `*Command` is never mutated — concurrent Execs (tests, embedded use) are safe.

## Testability

```go
func TestServe(t *testing.T) {
    var out, errOut bytes.Buffer
    root := newRoot()

    code := root.Exec([]string{"serve", "--port", "9090"},
        cli.WithStdout(&out),
        cli.WithStderr(&errOut),
    )
    if code != 0 {
        t.Fatalf("exit %d: %s", code, errOut.String())
    }
    // assert against out
}
```

`root.Parse(args)` is exposed too if you want to inspect the resolved command + values without executing the handler.

## Status

Alpha. Public API may change.
