package cli

import (
	"fmt"
	"slices"
	"strings"
)

// withInjectedCompletion returns a shallow copy of c with the
// built-in "completion" and "__complete" subcommands appended,
// when the caller has not already declared their own equivalents.
// The user's *Command is never mutated, so concurrent Exec calls
// (tests, embedded use) are safe and a caller inspecting their own
// tree after Exec sees exactly what they declared.
//
// The injected subcommands' Run closures capture the returned
// *Command, so __complete walks the assembled tree (and sees the
// auto-injected entries among its own suggestions).
func withInjectedCompletion(c *Command) *Command {
	if c == nil {
		return nil
	}
	root := *c
	root.Subcommands = slices.Clone(c.Subcommands)
	if !hasSub(&root, "completion") {
		root.Subcommands = append(root.Subcommands, completionCmd(&root))
	}
	if !hasSub(&root, "__complete") {
		root.Subcommands = append(root.Subcommands, completeCmd(&root))
	}
	return &root
}

func hasSub(c *Command, name string) bool {
	for _, s := range c.Subcommands {
		if s.Name == name {
			return true
		}
	}
	return false
}

// completionCmd builds the user-facing "completion" subcommand.
// `myapp completion bash > /etc/bash_completion.d/myapp`.
func completionCmd(root *Command) *Command {
	shellArg := StringArg("shell").
		Required().
		Help("bash | zsh | fish").
		OneOf("bash", "zsh", "fish")
	return &Command{
		Name: "completion",
		Help: "print a shell completion script",
		Long: "Emit a shell completion script for the given shell. " +
			"Source the output from your shell's rc file, or save it " +
			"to the appropriate per-shell completion directory.",
		// Auto-injected; kept out of the user's help and tab
		// suggestions so it does not pollute the command tree the
		// caller declared. Discoverable via the library's README.
		Hidden: true,
		Args:   Args(shellArg),
		Run: func(ctx Context) error {
			switch s := shellArg.Get(ctx); s {
			case "bash":
				fmt.Fprint(ctx.Stdout(), bashCompletion(root.Name))
			case "zsh":
				fmt.Fprint(ctx.Stdout(), zshCompletion(root.Name))
			case "fish":
				fmt.Fprint(ctx.Stdout(), fishCompletion(root.Name))
			default:
				// Unreachable while the OneOf above stays in sync
				// with the switch. Belt-and-braces error so a
				// future regression that widens OneOf (or drops
				// it) doesn't silently print nothing.
				return fmt.Errorf("internal: no emitter wired up for shell %q", s)
			}
			return nil
		},
	}
}

// completeCmd builds the hidden "__complete" subcommand the
// generated scripts invoke. Args are the partial token list (with
// the current word being completed as the trailing entry).
func completeCmd(root *Command) *Command {
	words := StringSliceArg("words").Variadic()
	return &Command{
		Name:   "__complete",
		Hidden: true,
		Args:   Args(words),
		Run: func(ctx Context) error {
			return runComplete(ctx, root, words.Get(ctx))
		},
	}
}

// pathWalk is the shared state every completion path needs after
// it has consumed the tokens that come before the current word.
type pathWalk struct {
	// cmd is the deepest command tokens resolved into.
	cmd *Command
	// known is the full flag set visible at cmd (parent flags
	// inherited).
	known []AnyFlag
	// positionalCount is the number of non-flag, non-subcommand
	// tokens already supplied. The next positional the user
	// types will land in cmd.Args[positionalCount] (or the
	// trailing variadic).
	positionalCount int
	// pendingFlag is non-nil when the last token in the walk was
	// a non-bool flag with no value yet, meaning the current
	// word being completed is that flag's value.
	pendingFlag AnyFlag
	// positionalsStarted mirrors the parser: once a non-flag,
	// non-subcommand token appears, subcommand dispatch stops.
	// runComplete uses this to suppress subcommand suggestions
	// after the first positional.
	positionalsStarted bool
	// positionalLocked stops flag parsing too, set only when
	// the resolved command has a variadic positional. Used so
	// completion does not suggest flags the parser would refuse
	// to accept (because they'd be swallowed by the variadic).
	positionalLocked bool
}

// walkPath replays the parser's subcommand-descent + flag-skipping
// logic against tokens, without actually parsing values. It is the
// shared front-end of every completion path: the inline
// `--foo=<TAB>` form reads .known; the main runComplete code reads
// every field. Centralising the walk here keeps the two callers
// from drifting (combined short flags, escape sequences, future
// parser tweaks all live in one place).
func walkPath(root *Command, tokens []string) pathWalk {
	w := pathWalk{
		cmd:   root,
		known: append([]AnyFlag(nil), root.Flags...),
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		// "--" separator: flag parsing stops; the token itself is
		// not a positional, but everything after it is. Matches
		// the parser so positionalCount stays consistent and
		// post-"--" flag-shaped tokens get bound to positionals.
		if tok == "--" {
			w.positionalsStarted = true
			w.positionalLocked = true
			continue
		}
		// Flag handling only applies while not locked. Locking
		// happens after the first positional only when the
		// resolved command has a variadic, same rule the parser
		// uses.
		if !w.positionalLocked && isFlagToken(tok) {
			if !strings.Contains(tok, "=") {
				if f := lookupFlagToken(w.known, tok); f != nil && !f.flagIsBool() {
					if i+1 < len(tokens) {
						i++ // consume the flag's value
						continue
					}
					// Flag is the last consumed token; its
					// value is what the user is typing now.
					w.pendingFlag = f
					continue
				}
			}
			continue
		}
		if !w.positionalsStarted {
			if sub := findSub(w.cmd, tok); sub != nil {
				w.cmd = sub
				w.known = append(w.known, sub.Flags...)
				continue
			}
		}
		w.positionalsStarted = true
		if commandHasVariadic(w.cmd) {
			w.positionalLocked = true
		}
		w.positionalCount++
	}
	return w
}

// runComplete walks the command tree against `partial` and emits
// candidate strings, one per line, to ctx.Stdout.
//
// Handled shapes:
//   - subcommand names at the resolved command level
//   - long flag names after "--<prefix>"
//   - short + long flag names after a bare "-"
//   - the value of a non-bool flag the user has just typed,
//     either after a space ("--mode <TAB>") or inline
//     ("--mode=<TAB>"), via the flag's [CompleteFn]
//   - the next positional argument's [CompleteFn], or the
//     trailing variadic arg's [CompleteFn] once positionals
//     have been exhausted
//
// Pre-filters by Word so dynamic candidate lists don't have to.
func runComplete(ctx Context, root *Command, partial []string) error {
	cur := ""
	rest := partial
	if len(partial) > 0 {
		cur = partial[len(partial)-1]
		rest = partial[:len(partial)-1]
	}
	w := walkPath(root, rest)

	emit := func(s string) { fmt.Fprintln(ctx.Stdout(), s) }
	emitCandidates := func(fn CompleteFn, valPrefix, displayPrefix string) {
		if fn == nil {
			return
		}
		for _, c := range fn(CompleteContext{Word: valPrefix, Args: partial}) {
			if strings.HasPrefix(c, valPrefix) {
				emit(displayPrefix + c)
			}
		}
	}

	// Flag-related completion fires while flags are still parsed
	// (i.e. !positionalLocked). Subcommand suggestions stop the
	// moment any positional appears (positionalsStarted), because
	// the parser would not descend after that point.
	if !w.positionalLocked {
		// --foo=<TAB> inline value completion. For bool flags we
		// suggest the literal true/false; otherwise we delegate
		// to the flag's CompleteFn.
		if strings.HasPrefix(cur, "--") && strings.Contains(cur, "=") {
			eq := strings.Index(cur, "=")
			name := strings.TrimPrefix(cur[:eq], "--")
			valPrefix := cur[eq+1:]
			if f := findFlagLong(w.known, name); f != nil {
				if f.flagIsBool() {
					for _, c := range []string{"true", "false"} {
						if strings.HasPrefix(c, valPrefix) {
							emit("--" + name + "=" + c)
						}
					}
					return nil
				}
				emitCandidates(f.flagCompleter(), valPrefix, "--"+name+"=")
				return nil
			}
		}

		// Mid-flag value completion ("--mode <TAB>" or "-m <TAB>").
		if w.pendingFlag != nil {
			emitCandidates(w.pendingFlag.flagCompleter(), cur, "")
			return nil
		}

		// Flag-name completion when the current word is flag-shaped.
		if strings.HasPrefix(cur, "--") {
			prefix := strings.TrimPrefix(cur, "--")
			for _, f := range w.known {
				if f.flagHidden() {
					continue
				}
				if strings.HasPrefix(f.flagName(), prefix) {
					emit("--" + f.flagName())
				}
			}
			return nil
		}
		if cur == "-" {
			for _, f := range w.known {
				if f.flagHidden() {
					continue
				}
				if f.flagShort() != 0 {
					emit("-" + string(f.flagShort()))
				}
				emit("--" + f.flagName())
			}
			return nil
		}
	}

	// Subcommand candidates only fire before the first positional:
	// once a positional appears the parser will not descend, so
	// suggesting subcommands would mislead.
	if !w.positionalsStarted {
		for _, s := range w.cmd.Subcommands {
			if s.Hidden {
				continue
			}
			if strings.HasPrefix(s.Name, cur) {
				emit(s.Name)
			}
		}
	}

	// Positional arg candidates: pick the arg slot we're about to
	// fill, or the trailing variadic if we've blown past the
	// declared count.
	switch {
	case w.positionalCount < len(w.cmd.Args):
		emitCandidates(w.cmd.Args[w.positionalCount].argCompleter(), cur, "")
	case len(w.cmd.Args) > 0:
		last := w.cmd.Args[len(w.cmd.Args)-1]
		if last.argVariadic() {
			emitCandidates(last.argCompleter(), cur, "")
		}
	}
	return nil
}

// lookupFlagToken resolves --name / -n / --name=val / -n=val to a
// known flag, for use during completion walking. Returns nil when
// the token doesn't match.
func lookupFlagToken(flags []AnyFlag, tok string) AnyFlag {
	body := strings.TrimLeft(tok, "-")
	if i := strings.Index(body, "="); i >= 0 {
		body = body[:i]
	}
	if strings.HasPrefix(tok, "--") {
		return findFlagLong(flags, body)
	}
	if len(body) == 1 {
		return findFlagShort(flags, []rune(body)[0])
	}
	return nil
}

// ---------- shell script emitters ----------

// bashCompletion returns a bash completion script that registers
// _<prog>_complete and binds it to <prog>. It calls back into the
// binary with the hidden __complete subcommand.
func bashCompletion(prog string) string {
	return `# bash completion for ` + prog + `
_` + prog + `_complete() {
    local cur words
    cur="${COMP_WORDS[COMP_CWORD]}"
    words=("${COMP_WORDS[@]:1:COMP_CWORD}")
    local IFS=$'\n'
    local out
    out=$(` + prog + ` __complete -- "${words[@]}" 2>/dev/null)
    COMPREPLY=( $(compgen -W "$out" -- "$cur") )
    return 0
}
complete -F _` + prog + `_complete ` + prog + `
`
}

func zshCompletion(prog string) string {
	return `#compdef ` + prog + `
_` + prog + `() {
    # NOTE: do NOT 'local words'; it shadows the special `+"`words`"+`
    # variable the completion machinery exposes from the calling
    # scope, leaving the slice empty. Use a differently-named local
    # to capture the slice instead.
    local -a candidates cwords
    cwords=("${(@)words[2,$CURRENT]}")
    candidates=("${(@f)$(` + prog + ` __complete -- "${cwords[@]}" 2>/dev/null)}")
    if [[ ${#candidates[@]} -gt 0 ]]; then
        _describe 'values' candidates
    fi
}
compdef _` + prog + ` ` + prog + `
`
}

func fishCompletion(prog string) string {
	return `# fish completion for ` + prog + `
function __` + prog + `_complete
    set -l words (commandline -opc) (commandline -ct)
    set -e words[1]
    ` + prog + ` __complete -- $words 2>/dev/null
end
complete -c ` + prog + ` -f -a '(__` + prog + `_complete)'
`
}
