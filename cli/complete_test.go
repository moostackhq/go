package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTimeFlag_ParsesRFC3339(t *testing.T) {
	when := TimeFlag("when")
	root := &Command{
		Name:  "myapp",
		Flags: Flags(when),
		Run:   func(Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"--when", "2024-03-15T10:30:00Z"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)
	// Construct a context just to read the value via Lookup.
	res, _ := root.Parse([]string{"--when", "2024-03-15T10:30:00Z"})
	got, ok := res.values[when]
	if !ok {
		t.Fatal("when not present in values")
	}
	if !got.(time.Time).Equal(want) {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestTimeFlag_RejectsNonRFC3339(t *testing.T) {
	when := TimeFlag("when")
	root := &Command{Name: "myapp", Flags: Flags(when), Run: func(Context) error { return nil }}
	code, _, stderr := exec(t, root, []string{"--when", "yesterday"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "--when") {
		t.Errorf("error should reference the flag, got %q", stderr)
	}
}

func TestCompletion_ScriptsEmitNonEmpty(t *testing.T) {
	root := &Command{Name: "myapp", Run: func(Context) error { return nil }}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			code, stdout, stderr := exec(t, root, []string{"completion", shell})
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(stdout, "myapp") {
				t.Errorf("%s script should mention program name, got %q", shell, stdout)
			}
			if !strings.Contains(stdout, "__complete") {
				t.Errorf("%s script should call back via __complete, got %q", shell, stdout)
			}
		})
	}
}

func TestCompletion_UnsupportedShell(t *testing.T) {
	root := &Command{Name: "myapp", Run: func(Context) error { return nil }}
	code, _, stderr := exec(t, root, []string{"completion", "pwsh"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	// OneOf validator catches this at parse time before the handler
	// runs. Error names the rejected value and the allowed set.
	if !strings.Contains(stderr, "pwsh") || !strings.Contains(stderr, "bash") {
		t.Errorf("stderr should name the rejected value and the allowed shells, got %q", stderr)
	}
}

func TestComplete_SubcommandCandidates(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
			{Name: "migrate", Run: func(Context) error { return nil }},
			{Name: "status", Run: func(Context) error { return nil }},
		},
	}
	// Empty current word → every user-declared subcommand. The
	// auto-injected `completion` and `__complete` are Hidden, so
	// neither appears in the candidate list.
	_, stdout, _ := exec(t, root, []string{"__complete", "--", ""})
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	want := map[string]bool{"serve": false, "migrate": false, "status": false}
	for _, l := range lines {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("completion should suggest %q (got %v)", name, lines)
		}
	}
	for _, hidden := range []string{"completion", "__complete"} {
		for _, l := range lines {
			if l == hidden {
				t.Errorf("auto-injected %q should not appear in candidate list (got %v)", hidden, lines)
			}
		}
	}
}

func TestComplete_PrefixFiltering(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
			{Name: "migrate", Run: func(Context) error { return nil }},
		},
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "se"})
	if !strings.Contains(stdout, "serve") {
		t.Errorf("want serve in output, got %q", stdout)
	}
	if strings.Contains(stdout, "migrate") {
		t.Errorf("migrate should not match prefix 'se', got %q", stdout)
	}
}

func TestComplete_LongFlagCandidates(t *testing.T) {
	port := IntFlag("port").Default(8080)
	verbose := BoolFlag("verbose")
	root := &Command{Name: "myapp", Flags: Flags(port, verbose), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--p"})
	if !strings.Contains(stdout, "--port") {
		t.Errorf("want --port suggestion, got %q", stdout)
	}
	if strings.Contains(stdout, "--verbose") {
		t.Errorf("--verbose should not match prefix --p, got %q", stdout)
	}
}

func TestComplete_DashSuggestsAllFlags(t *testing.T) {
	port := IntFlag("port").Short('p').Default(8080)
	verbose := BoolFlag("verbose").Short('v')
	root := &Command{Name: "myapp", Flags: Flags(port, verbose), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "-"})
	for _, want := range []string{"-p", "--port", "-v", "--verbose"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}
}

func TestComplete_FlagValueAfterSpace(t *testing.T) {
	mode := StringFlag("mode").OneOf("dev", "staging", "prod")
	root := &Command{Name: "myapp", Flags: Flags(mode), Run: func(Context) error { return nil }}

	// "myapp --mode " (trailing empty word) should suggest
	// every OneOf value.
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--mode", ""})
	for _, want := range []string{"dev", "staging", "prod"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}

	// "myapp --mode s" should narrow to staging.
	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--mode", "s"})
	if !strings.Contains(stdout, "staging") {
		t.Errorf("want staging in suggestions, got %q", stdout)
	}
	if strings.Contains(stdout, "dev") || strings.Contains(stdout, "prod") {
		t.Errorf("only 'staging' should match prefix 's', got %q", stdout)
	}
}

func TestComplete_FlagValueInline(t *testing.T) {
	mode := StringFlag("mode").OneOf("dev", "staging", "prod")
	root := &Command{Name: "myapp", Flags: Flags(mode), Run: func(Context) error { return nil }}

	// "myapp --mode=" (inline form, empty value): every value
	// emitted prefixed with --mode= so the shell replaces the
	// whole token.
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--mode="})
	for _, want := range []string{"--mode=dev", "--mode=staging", "--mode=prod"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}

	// "myapp --mode=s" should narrow.
	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--mode=s"})
	if !strings.Contains(stdout, "--mode=staging") {
		t.Errorf("want --mode=staging, got %q", stdout)
	}
	if strings.Contains(stdout, "--mode=dev") || strings.Contains(stdout, "--mode=prod") {
		t.Errorf("only --mode=staging should match prefix s, got %q", stdout)
	}
}

func TestComplete_FlagValueShortForm(t *testing.T) {
	mode := StringFlag("mode").Short('m').OneOf("dev", "staging", "prod")
	root := &Command{Name: "myapp", Flags: Flags(mode), Run: func(Context) error { return nil }}

	// "myapp -m ", short flag, trailing empty value to complete.
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "-m", ""})
	for _, want := range []string{"dev", "staging", "prod"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}
}

func TestComplete_VariadicLocksFlagsAfterPositional(t *testing.T) {
	// ssh-style: a variadic positional swallows every later token,
	// including flag-shaped ones. The parser sets positionalLocked,
	// completion must mirror that, suggesting --port here would
	// mislead the user since the parser would treat it as a value
	// for the variadic.
	port := IntFlag("port").Default(22)
	root := &Command{
		Name:  "ssh",
		Flags: Flags(port),
		Args:  Args(StringArg("host").Required(), StringSliceArg("extra").Variadic()),
		Run:   func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "example.com", "--p"})
	if strings.Contains(stdout, "--port") {
		t.Errorf("variadic command: --port should NOT be suggested after positional, got %q", stdout)
	}
}

func TestComplete_NonVariadicAllowsFlagsAfterPositional(t *testing.T) {
	// Universal CLI convention: a non-variadic command interleaves
	// flags and positionals freely. The parser accepts
	// `serve host --port 80`; completion must keep suggesting
	// flags after a positional so the user can discover them.
	port := IntFlag("port").Default(8080)
	root := &Command{
		Name:  "serve",
		Flags: Flags(port),
		Args:  Args(StringArg("host").Required()),
		Run:   func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "example.com", "--p"})
	if !strings.Contains(stdout, "--port") {
		t.Errorf("non-variadic command: --port should still be suggested after positional, got %q", stdout)
	}
}

func TestParse_FlagAfterPositional_NonVariadic(t *testing.T) {
	// The parser-level counterpart: flags MUST be accepted after
	// a positional when the command has no variadic. This is the
	// `todo add "buy milk" --priority high` case.
	port := IntFlag("port").Default(8080)
	host := StringArg("host").Required()
	root := &Command{
		Name:  "serve",
		Flags: Flags(port),
		Args:  Args(host),
		Run: func(ctx Context) error {
			fmt.Fprintf(ctx.Stdout(), "host=%s port=%d\n", host.Get(ctx), port.Get(ctx))
			return nil
		},
	}
	code, stdout, stderr := exec(t, root, []string{"example.com", "--port", "9090"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "host=example.com port=9090") {
		t.Errorf("unexpected stdout: %q", stdout)
	}
}

func TestComplete_PositionalLocked_KeepsPositionalSuggestions(t *testing.T) {
	// Variadic case: positional candidates should keep flowing
	// even after the first positional, because the variadic
	// accepts more.
	rest := StringSliceArg("rest").Variadic().Complete(StaticValues("alpha", "bravo"))
	root := &Command{
		Name: "myapp",
		Args: Args(rest),
		Run:  func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "alpha", "b"})
	if !strings.Contains(stdout, "bravo") {
		t.Errorf("variadic completer should still suggest, got %q", stdout)
	}
}

func TestComplete_HonoursDoubleDashSeparator(t *testing.T) {
	// After "--", every token is positional and the "--" itself
	// is not counted. The walker must agree with the parser on
	// which positional slot the current word is filling.
	a := StringArg("a").Required().Complete(StaticValues("alpha-only"))
	b := StringArg("b").Required().Complete(StaticValues("bravo-only"))
	root := &Command{
		Name: "myapp",
		Args: Args(a, b),
		Run:  func(Context) error { return nil },
	}

	// "myapp -- arg1 <TAB>": cur targets slot b (the second
	// arg). The "--" is not a positional, so the suggestion is
	// for b, not c.
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--", "arg1", ""})
	if !strings.Contains(stdout, "bravo-only") {
		t.Errorf("after `-- arg1` cur should target arg b, got %q", stdout)
	}
	if strings.Contains(stdout, "alpha-only") {
		t.Errorf("slot a is already filled, should not suggest, got %q", stdout)
	}

	// "myapp -- <TAB>": cur targets slot a. Walker must NOT
	// have miscounted the "--" as a positional itself.
	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--", ""})
	if !strings.Contains(stdout, "alpha-only") {
		t.Errorf("after `--` alone cur should target arg a, got %q", stdout)
	}
}

func TestComplete_BoolFlagInlineSuggestsTrueFalse(t *testing.T) {
	verbose := BoolFlag("verbose")
	root := &Command{Name: "myapp", Flags: Flags(verbose), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--verbose="})
	for _, want := range []string{"--verbose=true", "--verbose=false"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}

	// Prefix filtering: "--verbose=t" narrows to true.
	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--verbose=t"})
	if !strings.Contains(stdout, "--verbose=true") {
		t.Errorf("want --verbose=true, got %q", stdout)
	}
	if strings.Contains(stdout, "--verbose=false") {
		t.Errorf("--verbose=false should not match prefix 't', got %q", stdout)
	}
}

func TestComplete_ArgCompleter(t *testing.T) {
	shell := StringArg("shell").Complete(StaticValues("bash", "zsh", "fish"))
	root := &Command{
		Name: "myapp",
		Args: Args(shell),
		Run:  func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "b"})
	if !strings.Contains(stdout, "bash") {
		t.Errorf("want bash suggestion for prefix 'b', got %q", stdout)
	}
	if strings.Contains(stdout, "zsh") {
		t.Errorf("zsh should not match prefix 'b', got %q", stdout)
	}
}

func TestHelp_AutoInjectedCompletionIsHidden(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Help: "run the server", Run: func(Context) error { return nil }},
		},
	}
	_, stdout, _ := exec(t, root, []string{"--help"})
	if !strings.Contains(stdout, "serve") {
		t.Errorf("user's subcommand should appear in help, got:\n%s", stdout)
	}
	for _, hidden := range []string{"completion", "__complete"} {
		if strings.Contains(stdout, hidden) {
			t.Errorf("auto-injected %q should not appear in help, got:\n%s", hidden, stdout)
		}
	}
}

func TestExec_DoesNotMutateUserCommandTree(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	before := len(root.Subcommands)
	beforeAddr := &root.Subcommands[0]

	exec(t, root, []string{"serve"})
	exec(t, root, []string{"serve"})
	exec(t, root, []string{"serve"})

	if len(root.Subcommands) != before {
		t.Errorf("Exec mutated user's Subcommands (was %d, now %d)", before, len(root.Subcommands))
	}
	if &root.Subcommands[0] != beforeAddr {
		t.Errorf("Exec mutated the user's Subcommands slice backing array")
	}
}

func TestExec_ConcurrentInvocationsAreSafe(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	const workers = 16
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			exec(t, root, []string{"serve"})
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	// The user's tree must be untouched regardless of how many
	// goroutines ran concurrently.
	if len(root.Subcommands) != 1 {
		t.Errorf("concurrent Exec mutated user's tree: %d subcommands", len(root.Subcommands))
	}
}

func TestHelp_NoColorOnBuffer(t *testing.T) {
	// bytes.Buffer is not a *os.File so detectTermInfo should
	// disable colour. Help output must contain no ANSI escapes.
	root := &Command{
		Name:  "myapp",
		Flags: Flags(IntFlag("port").Default(8080)),
		Run:   func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"--help"})
	if strings.Contains(stdout, "\033[") {
		t.Errorf("non-terminal writer should never emit ANSI escapes, got:\n%s", stdout)
	}
}
