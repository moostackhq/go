package cli

import (
	"fmt"
	"strings"
)

// parseResult is the outcome of [parseArgs]. The caller (exec.go)
// decides what to do with it: render help, render version, dispatch
// the handler.
type parseResult struct {
	cmd     *Command   // command to run (deepest matched)
	path    []*Command // root … cmd
	values  values     // parsed flag + positional values
	help    bool       // --help / -h on the path
	version bool       // --version on the path
}

// parseArgs walks tokens against root's command tree. env is the
// environment lookup used to resolve flags bound via Env(); passing
// os.LookupEnv connects it to the real process environment, tests
// pass a fake.
//
// On any error the partial result is still returned so callers
// (specifically the default error renderer) can show help for the
// deepest command they successfully reached.
func parseArgs(root *Command, tokens []string, env func(string) (string, bool)) (*parseResult, []AnyFlag, error) {
	if err := validateArgsDecl(root); err != nil {
		return &parseResult{cmd: root, path: []*Command{root}}, root.Flags, err
	}

	res := &parseResult{
		cmd:    root,
		path:   []*Command{root},
		values: values{},
	}
	known := append([]AnyFlag(nil), root.Flags...)
	positional := []string(nil)
	// positionalsStarted: once a non-flag, non-subcommand token
	// appears, subcommand dispatch stops; the first positional
	// nails down which command we're in.
	//
	// positionalLocked: stops flag parsing as well, so a trailing
	// variadic can swallow flag-shaped tokens (`ssh host ls -l`).
	// We only set this when the resolved command actually has a
	// variadic positional; otherwise flags continue to interleave
	// freely with positionals, which is the universal CLI
	// expectation (`todo add "buy milk" --priority high`).
	positionalsStarted := false
	positionalLocked := false

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		// "--" terminates flag parsing; everything after is positional.
		if tok == "--" {
			positional = append(positional, tokens[i+1:]...)
			break
		}

		// --help / -h short-circuits everything.
		if tok == "--help" || tok == "-h" {
			res.help = true
			return res, known, nil
		}

		// --version is only respected if the root (or current cmd)
		// has a Version string. Otherwise it's just an unknown flag.
		if tok == "--version" && res.cmd.Version != "" {
			res.version = true
			return res, known, nil
		}

		// Flag forms (--name, --name=value, -n, -n=value).
		if !positionalLocked && isFlagToken(tok) {
			next := ""
			haveNext := i+1 < len(tokens)
			if haveNext {
				next = tokens[i+1]
			}
			consumed, err := applyFlagToken(tok, next, haveNext, known, res.values)
			if err != nil {
				return res, known, err
			}
			i += consumed
			continue
		}

		// Non-flag token: either subcommand (only before the first
		// positional has been collected) or positional.
		if !positionalsStarted {
			if sub := findSub(res.cmd, tok); sub != nil {
				res.cmd = sub
				res.path = append(res.path, sub)
				known = append(known, sub.Flags...)
				continue
			}
			// No subcommand match. If the current command can't run
			// on its own AND has subcommands, this is a typo.
			if res.cmd.Run == nil && len(res.cmd.Subcommands) > 0 {
				return res, known, unknownSubcommand(res.cmd, tok)
			}
		}
		positionalsStarted = true
		if commandHasVariadic(res.cmd) {
			positionalLocked = true
		}
		positional = append(positional, tok)
	}

	// Env fallback for flags not explicitly set.
	for _, f := range known {
		if err := f.flagApplyEnv(res.values, env); err != nil {
			return res, known, err
		}
	}

	// Required + validation for flags.
	for _, f := range known {
		if f.flagRequired() && !f.flagPresent(res.values) && !f.flagHasDefault() {
			return res, known, fmt.Errorf("required flag --%s not set: %w", f.flagName(), ErrUsage)
		}
		if err := f.flagValidate(res.values); err != nil {
			return res, known, err
		}
	}

	// If the matched command is grouping-only and the user did not
	// supply a subcommand, surface that as the bare
	// ErrMissingSubcommand sentinel. DefaultOnError renders the
	// command's help to stderr without a separate error message
	// line, which is the conventional UX for "show me what's
	// available" rather than "you typed something wrong."
	if res.cmd.Run == nil && len(res.cmd.Subcommands) > 0 {
		return res, known, ErrMissingSubcommand
	}

	// Bind positionals to declared args.
	if err := bindPositionals(res.cmd.Args, positional, res.values); err != nil {
		return res, known, err
	}
	for _, a := range res.cmd.Args {
		if err := a.argValidate(res.values); err != nil {
			return res, known, err
		}
	}

	return res, known, nil
}

// validateArgsDecl checks declaration-time invariants for the args
// of every command in the tree:
//   - variadic args must be last
//   - required args must precede optional args
//   - slice-typed args must be Variadic (a non-variadic slice arg
//     would receive exactly one token, which is never what the
//     caller meant)
func validateArgsDecl(c *Command) error {
	seenOptional := false
	for i, a := range c.Args {
		if a.argVariadic() && i != len(c.Args)-1 {
			return fmt.Errorf("command %q: variadic arg %q must be last", c.Name, a.argName())
		}
		if a.argRequired() && seenOptional {
			return fmt.Errorf("command %q: required arg %q must precede optional args", c.Name, a.argName())
		}
		if a.argIsSlice() && !a.argVariadic() {
			return fmt.Errorf("command %q: slice arg %q must be declared Variadic", c.Name, a.argName())
		}
		if !a.argRequired() && !a.argVariadic() {
			seenOptional = true
		}
	}
	for _, s := range c.Subcommands {
		if err := validateArgsDecl(s); err != nil {
			return err
		}
	}
	return nil
}

func findSub(c *Command, name string) *Command {
	for _, s := range c.Subcommands {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// commandHasVariadic reports whether c declares a variadic
// positional. The parser uses this to decide whether the first
// positional should also stop flag parsing: only commands with a
// variadic need that protection (so their tail catches flag-shaped
// tokens unchanged). Everything else keeps flags and positionals
// interleavable.
func commandHasVariadic(c *Command) bool {
	for _, a := range c.Args {
		if a.argVariadic() {
			return true
		}
	}
	return false
}

func findFlagLong(flags []AnyFlag, name string) AnyFlag {
	for _, f := range flags {
		if f.flagName() == name {
			return f
		}
	}
	return nil
}

func findFlagShort(flags []AnyFlag, r rune) AnyFlag {
	for _, f := range flags {
		if f.flagShort() == r && r != 0 {
			return f
		}
	}
	return nil
}

func isFlagToken(tok string) bool {
	return len(tok) >= 2 && tok[0] == '-' && tok != "--"
}

// applyFlagToken parses one flag token (and possibly consumes the
// next token as its value). Returns the number of EXTRA tokens
// consumed beyond tok (0 or 1).
func applyFlagToken(tok, next string, haveNext bool, flags []AnyFlag, vs values) (int, error) {
	isLong := strings.HasPrefix(tok, "--")
	body := tok[1:]
	if isLong {
		body = tok[2:]
	}

	name, valIn, hasInline := body, "", false
	if i := strings.Index(body, "="); i >= 0 {
		name = body[:i]
		valIn = body[i+1:]
		hasInline = true
	}

	var f AnyFlag
	if isLong {
		f = findFlagLong(flags, name)
		if f == nil {
			return 0, fmt.Errorf("unknown flag --%s: %w", name, ErrUsage)
		}
	} else {
		if len(name) == 0 {
			// bare "-" treated as positional by caller
			return 0, fmt.Errorf("bare '-' is not a valid flag: %w", ErrUsage)
		}
		// Only single-char short flags supported. -abc grouping
		// (cobra/getopt style) is deliberately not implemented in
		// v1: it surprises generic Get users when -a takes a value.
		r := []rune(name)[0]
		if len([]rune(name)) > 1 && !hasInline {
			return 0, fmt.Errorf("unknown flag -%s (combined short flags not supported): %w", name, ErrUsage)
		}
		f = findFlagShort(flags, r)
		if f == nil {
			return 0, fmt.Errorf("unknown flag -%s: %w", string(r), ErrUsage)
		}
	}

	if f.flagIsBool() {
		val := "true"
		if hasInline {
			val = valIn
		}
		return 0, f.flagApplyString(vs, val)
	}
	if hasInline {
		return 0, f.flagApplyString(vs, valIn)
	}
	if !haveNext {
		return 0, fmt.Errorf("--%s: missing value: %w", f.flagName(), ErrUsage)
	}
	return 1, f.flagApplyString(vs, next)
}

// bindPositionals walks the declared args in order, pulling
// tokens off the front of positional for each. A variadic last arg
// captures every remaining token.
func bindPositionals(args []AnyArg, positional []string, vs values) error {
	idx := 0
	for _, a := range args {
		if a.argVariadic() {
			for idx < len(positional) {
				if err := a.argApplyString(vs, positional[idx]); err != nil {
					return err
				}
				idx++
			}
			break
		}
		if idx >= len(positional) {
			if a.argRequired() {
				return fmt.Errorf("missing argument <%s>: %w", a.argName(), ErrMissingArg)
			}
			continue
		}
		if err := a.argApplyString(vs, positional[idx]); err != nil {
			return err
		}
		idx++
	}
	if idx < len(positional) {
		return fmt.Errorf("unexpected argument %q: %w", positional[idx], ErrUsage)
	}
	return nil
}

func commandNames(path []*Command) []string {
	out := make([]string, len(path))
	for i, c := range path {
		out[i] = c.Name
	}
	return out
}

// unknownSubcommand returns an error wrapping ErrUnknownCmd with a
// "did you mean ...?" suggestion when one is close enough.
func unknownSubcommand(c *Command, attempted string) error {
	suggestion := suggestSub(c, attempted)
	if suggestion != "" {
		return fmt.Errorf("unknown subcommand %q (did you mean %q?): %w", attempted, suggestion, ErrUnknownCmd)
	}
	return fmt.Errorf("unknown subcommand %q: %w", attempted, ErrUnknownCmd)
}

// suggestSub picks the closest non-hidden subcommand name by
// Levenshtein distance, returning "" if nothing is within a sane
// distance budget.
func suggestSub(c *Command, attempted string) string {
	best := ""
	bestDist := len(attempted) // anything not better than this is useless
	if bestDist > 4 {
		bestDist = 4
	}
	for _, s := range c.Subcommands {
		if s.Hidden {
			continue
		}
		d := levenshtein(attempted, s.Name)
		if d < bestDist {
			best = s.Name
			bestDist = d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
