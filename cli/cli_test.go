package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// noEnv is a stub passed via withEnv so tests don't accidentally
// pick up the developer's real environment.
func noEnv(string) (string, bool) { return "", false }

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// exec is the test entry point. Returns exit code, captured stdout,
// captured stderr.
func exec(t *testing.T, root *Command, args []string, opts ...ExecOption) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]ExecOption{
		WithStdout(&out),
		WithStderr(&errOut),
		withEnv(noEnv),
		WithSignalContext(context.Background()),
	}, opts...)
	code := root.Exec(args, full...)
	return code, out.String(), errOut.String()
}

// --- flag parsing ---

func TestFlag_LongAndShort(t *testing.T) {
	port := IntFlag("port").Short('p').Default(8080)
	verbose := BoolFlag("verbose").Short('v')
	root := &Command{
		Name:  "myapp",
		Flags: Flags(port, verbose),
		Run: func(ctx Context) error {
			fmt.Fprintf(ctx.Stdout(), "port=%d verbose=%v\n", port.Get(ctx), verbose.Get(ctx))
			return nil
		},
	}

	cases := []struct {
		args []string
		want string
	}{
		{[]string{}, "port=8080 verbose=false\n"},
		{[]string{"--port", "9090"}, "port=9090 verbose=false\n"},
		{[]string{"--port=9090"}, "port=9090 verbose=false\n"},
		{[]string{"-p", "9090"}, "port=9090 verbose=false\n"},
		{[]string{"-p=9090"}, "port=9090 verbose=false\n"},
		{[]string{"--verbose"}, "port=8080 verbose=true\n"},
		{[]string{"-v"}, "port=8080 verbose=true\n"},
		{[]string{"--verbose=false"}, "port=8080 verbose=false\n"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			code, stdout, stderr := exec(t, root, tc.args)
			if code != 0 {
				t.Fatalf("exit %d (stderr=%q)", code, stderr)
			}
			if stdout != tc.want {
				t.Errorf("stdout: want %q, got %q", tc.want, stdout)
			}
		})
	}
}

func TestFlag_Required(t *testing.T) {
	name := StringFlag("name").Required()
	root := &Command{
		Name:  "myapp",
		Flags: Flags(name),
		Run:   func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "required flag --name") {
		t.Errorf("stderr should mention required flag, got %q", stderr)
	}
}

func TestFlag_RequiredWithDefault_DoesNotError(t *testing.T) {
	// A default makes "required" trivially satisfied.
	port := IntFlag("port").Required().Default(8080)
	root := &Command{
		Name:  "myapp",
		Flags: Flags(port),
		Run: func(ctx Context) error {
			if port.Get(ctx) != 8080 {
				t.Errorf("want 8080, got %d", port.Get(ctx))
			}
			return nil
		},
	}
	if code, _, _ := exec(t, root, []string{}); code != 0 {
		t.Errorf("want exit 0, got %d", code)
	}
}

func TestFlag_Env(t *testing.T) {
	port := IntFlag("port").Env("PORT").Default(8080)
	root := &Command{
		Name:  "myapp",
		Flags: Flags(port),
		Run: func(ctx Context) error {
			fmt.Fprintf(ctx.Stdout(), "port=%d\n", port.Get(ctx))
			return nil
		},
	}
	_, stdout, _ := exec(t, root, []string{}, withEnv(envOf(map[string]string{"PORT": "7777"})))
	if !strings.Contains(stdout, "port=7777") {
		t.Errorf("env not applied: %q", stdout)
	}
	// Explicit flag beats env.
	_, stdout, _ = exec(t, root, []string{"--port", "9090"}, withEnv(envOf(map[string]string{"PORT": "7777"})))
	if !strings.Contains(stdout, "port=9090") {
		t.Errorf("explicit flag should win, got %q", stdout)
	}
}

func TestFlag_Validate_OneOf(t *testing.T) {
	level := StringFlag("level").OneOf("debug", "info", "warn", "error").Default("info")
	root := &Command{
		Name:  "myapp",
		Flags: Flags(level),
		Run:   func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"--level", "trace"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "not one of") {
		t.Errorf("stderr should mention validation, got %q", stderr)
	}
}

func TestFlag_Slice(t *testing.T) {
	items := StringSliceFlag("item")
	root := &Command{
		Name:  "myapp",
		Flags: Flags(items),
		Run: func(ctx Context) error {
			fmt.Fprintln(ctx.Stdout(), strings.Join(items.Get(ctx), ","))
			return nil
		},
	}
	_, stdout, _ := exec(t, root, []string{"--item", "a", "--item", "b", "--item=c"})
	if strings.TrimSpace(stdout) != "a,b,c" {
		t.Errorf("slice flag accumulation: %q", stdout)
	}
}

func TestFlag_Lookup_DistinguishesExplicitFromDefault(t *testing.T) {
	to := IntFlag("to").Default(0)
	root := &Command{
		Name:  "myapp",
		Flags: Flags(to),
		Run: func(ctx Context) error {
			if _, set := to.Lookup(ctx); set {
				fmt.Fprintln(ctx.Stdout(), "set")
			} else {
				fmt.Fprintln(ctx.Stdout(), "default")
			}
			return nil
		},
	}
	_, stdout, _ := exec(t, root, []string{})
	if strings.TrimSpace(stdout) != "default" {
		t.Errorf("want default, got %q", stdout)
	}
	_, stdout, _ = exec(t, root, []string{"--to", "0"})
	if strings.TrimSpace(stdout) != "set" {
		t.Errorf("want set, got %q", stdout)
	}
}

// --- subcommands ---

func TestSubcommand_RoutingAndInheritedFlags(t *testing.T) {
	dsn := StringFlag("dsn").Required()
	var seen string
	migrate := &Command{
		Name: "migrate",
		Run: func(ctx Context) error {
			seen = dsn.Get(ctx)
			return nil
		},
	}
	db := &Command{
		Name:        "db",
		Flags:       Flags(dsn),
		Subcommands: []*Command{migrate},
	}
	root := &Command{Name: "myapp", Subcommands: []*Command{db}}

	code, _, stderr := exec(t, root, []string{"db", "migrate", "--dsn", "postgres://x"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if seen != "postgres://x" {
		t.Errorf("parent flag not inherited, got %q", seen)
	}

	// Flag declared on parent can also be supplied before the subcommand.
	code, _, _ = exec(t, root, []string{"db", "--dsn", "postgres://y", "migrate"})
	if code != 0 {
		t.Errorf("flag-before-subcommand: exit %d", code)
	}
	if seen != "postgres://y" {
		t.Errorf("want y, got %q", seen)
	}
}

func TestSubcommand_GroupingOnlyShowsHelpOnBareInvocation(t *testing.T) {
	db := &Command{
		Name:        "db",
		Subcommands: []*Command{{Name: "status", Run: func(Context) error { return nil }}},
	}
	root := &Command{Name: "myapp", Subcommands: []*Command{db}}

	code, _, stderr := exec(t, root, []string{"db"})
	if code != 2 {
		t.Errorf("grouping-only bare exec: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("help should be rendered, got %q", stderr)
	}
	// Conventional UX: no "myapp: ..." error line above the
	// help. The user typed an incomplete command, not a wrong
	// one; the help itself is the answer.
	if strings.Contains(stderr, "myapp:") || strings.Contains(stderr, "missing subcommand") {
		t.Errorf("grouping-only bare exec should not print an error line, got %q", stderr)
	}
}

func TestSubcommand_TypoSuggestion(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "migrate", Run: func(Context) error { return nil }},
			{Name: "status", Run: func(Context) error { return nil }},
		},
	}
	code, _, stderr := exec(t, root, []string{"migrat"})
	if code != 2 {
		t.Errorf("unknown subcommand: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "did you mean \"migrate\"?") {
		t.Errorf("want typo suggestion, got %q", stderr)
	}
}

// --- positional args ---

func TestArgs_RequiredAndVariadic(t *testing.T) {
	host := StringArg("host").Required()
	extra := StringSliceArg("extra").Variadic()
	root := &Command{
		Name: "ssh",
		Args: Args(host, extra),
		Run: func(ctx Context) error {
			fmt.Fprintf(ctx.Stdout(), "host=%s extra=%v\n", host.Get(ctx), extra.Get(ctx))
			return nil
		},
	}

	code, stdout, stderr := exec(t, root, []string{"example.com", "ls", "-l"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "host=example.com") || !strings.Contains(stdout, "extra=[ls -l]") {
		t.Errorf("unexpected stdout: %q", stdout)
	}

	code, _, stderr = exec(t, root, []string{})
	if code != 2 {
		t.Errorf("missing required arg: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "missing argument") {
		t.Errorf("want missing-arg error, got %q", stderr)
	}
}

func TestArg_OneOf(t *testing.T) {
	mode := StringArg("mode").OneOf("dev", "staging", "prod").Required()
	root := &Command{
		Name: "myapp",
		Args: Args(mode),
		Run: func(ctx Context) error {
			fmt.Fprintln(ctx.Stdout(), mode.Get(ctx))
			return nil
		},
	}
	if code, stdout, _ := exec(t, root, []string{"staging"}); code != 0 || strings.TrimSpace(stdout) != "staging" {
		t.Errorf("accepted value: code=%d stdout=%q", code, stdout)
	}
	code, _, stderr := exec(t, root, []string{"qa"})
	if code != 2 {
		t.Errorf("disallowed value: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "not one of") {
		t.Errorf("error should mention validation, got %q", stderr)
	}
}

func TestArgs_UnexpectedExtras(t *testing.T) {
	host := StringArg("host").Required()
	root := &Command{
		Name: "ssh",
		Args: Args(host),
		Run:  func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"a", "b"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("want unexpected-arg error, got %q", stderr)
	}
}

func TestArgs_SliceMustBeVariadic(t *testing.T) {
	// A slice arg without .Variadic() would only ever receive one
	// token, almost certainly a programming mistake. Caught at
	// declaration time on first Exec.
	root := &Command{
		Name: "bad",
		Args: Args(StringSliceArg("rest")),
		Run:  func(Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"x"})
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "must be declared Variadic") {
		t.Errorf("want declaration-error message, got %q", stderr)
	}

	// Adding .Variadic() makes it valid.
	rest := StringSliceArg("rest").Variadic()
	good := &Command{
		Name: "good",
		Args: Args(rest),
		Run: func(ctx Context) error {
			fmt.Fprintln(ctx.Stdout(), strings.Join(rest.Get(ctx), ","))
			return nil
		},
	}
	if code, stdout, _ := exec(t, good, []string{"a", "b", "c"}); code != 0 || strings.TrimSpace(stdout) != "a,b,c" {
		t.Errorf("variadic slice arg: code=%d stdout=%q", code, stdout)
	}
}

func TestArgs_VariadicMustBeLast(t *testing.T) {
	// Declaration error, surfaced at exec time, exit 1 (programmer
	// bug, not a user-fixable invocation issue).
	root := &Command{
		Name: "bad",
		Args: Args(StringSliceArg("rest").Variadic(), StringArg("after")),
		Run:  func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"x"})
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "variadic arg") {
		t.Errorf("want declaration-error message, got %q", stderr)
	}
}

// --- help / version ---

func TestHelp_RootRendersFlagsAndSubcommands(t *testing.T) {
	port := IntFlag("port").Default(8080).Help("port to listen on")
	root := &Command{
		Name:  "myapp",
		Help:  "do useful things",
		Flags: Flags(port),
		Subcommands: []*Command{
			{Name: "serve", Help: "run the server", Run: func(Context) error { return nil }},
		},
	}
	code, stdout, _ := exec(t, root, []string{"--help"})
	if code != 0 {
		t.Errorf("want exit 0, got %d", code)
	}
	for _, want := range []string{"Usage:", "myapp", "--port", "port to listen on", "Commands:", "serve"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help should contain %q, got:\n%s", want, stdout)
		}
	}
}

func TestVersion(t *testing.T) {
	root := &Command{Name: "myapp", Version: "1.2.3", Run: func(Context) error { return nil }}
	code, stdout, _ := exec(t, root, []string{"--version"})
	if code != 0 {
		t.Errorf("want exit 0, got %d", code)
	}
	if strings.TrimSpace(stdout) != "1.2.3" {
		t.Errorf("want 1.2.3, got %q", stdout)
	}
}

// --- middleware ---

func TestMiddleware_DeclarationOrder(t *testing.T) {
	var log []string
	mark := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(ctx Context) error {
				log = append(log, "enter "+name)
				err := next(ctx)
				log = append(log, "leave "+name)
				return err
			}
		}
	}
	root := &Command{
		Name: "myapp",
		Use:  []Middleware{mark("A"), mark("B")},
		Run: func(ctx Context) error {
			log = append(log, "handler")
			return nil
		},
	}
	exec(t, root, []string{})
	want := []string{"enter A", "enter B", "handler", "leave B", "leave A"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("middleware order:\nwant %v\ngot  %v", want, log)
	}
}

func TestMiddleware_InheritedFromParent(t *testing.T) {
	var hit bool
	mark := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(ctx Context) error { hit = true; return next(ctx) }
		}
	}
	child := &Command{Name: "child", Run: func(Context) error { return nil }}
	root := &Command{
		Name:        "myapp",
		Use:         []Middleware{mark("root")},
		Subcommands: []*Command{child},
	}
	exec(t, root, []string{"child"})
	if !hit {
		t.Error("parent middleware should run for child commands")
	}
}

// --- error handling / context ---

func TestExec_HandlerError_RendersAsExit1(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run: func(Context) error { return errors.New("boom") },
	}
	code, _, stderr := exec(t, root, []string{})
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "myapp: boom") {
		t.Errorf("stderr should be prefixed with program name: %q", stderr)
	}
}

func TestExec_CustomOnError(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run:  func(Context) error { return errors.New("boom") },
	}
	code, _, stderr := exec(t, root, []string{},
		OnError(func(ctx Context, err error) int {
			fmt.Fprintf(ctx.Stderr(), "CUSTOM:%s", err)
			return 42
		}),
	)
	if code != 42 {
		t.Errorf("want exit 42, got %d", code)
	}
	if !strings.Contains(stderr, "CUSTOM:boom") {
		t.Errorf("want CUSTOM:boom, got %q", stderr)
	}
}

func TestExec_ContextCanceledExitCode(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run:  func(Context) error { return context.Canceled },
	}
	code, _, _ := exec(t, root, []string{})
	if code != 130 {
		t.Errorf("want exit 130, got %d", code)
	}
}

func TestExec_UsageErrorTriggersHelpAppend(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run:  func(Context) error { return UsageError("you did it wrong") },
	}
	code, _, stderr := exec(t, root, []string{})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("usage error should append help: %q", stderr)
	}
}

func TestExec_CtxCancellationPropagates(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	root := &Command{
		Name: "myapp",
		Run: func(ctx Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				return errors.New("never reached")
			}
		},
	}
	code, _, _ := exec(t, root, []string{}, WithSignalContext(parent))
	if code != 130 {
		t.Errorf("want exit 130 for cancelled ctx, got %d", code)
	}
}

// --- parse-without-execute ---

func TestParse_StandaloneInspection(t *testing.T) {
	port := IntFlag("port").Default(8080)
	serve := &Command{Name: "serve", Flags: Flags(port), Run: func(Context) error { return nil }}
	root := &Command{Name: "myapp", Subcommands: []*Command{serve}}

	res, err := root.Parse([]string{"serve", "--port", "9090"})
	if err != nil {
		t.Fatal(err)
	}
	if res.cmd != serve {
		t.Errorf("resolved wrong command")
	}
	if got := res.values[port]; got != 9090 {
		t.Errorf("port: want 9090, got %v", got)
	}
}
