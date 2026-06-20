package config

import (
	"fmt"
	"sort"
	"strings"
)

// Problem is one thing wrong with the configuration: an invalid default,
// a bad YAML value, an unparseable env override, or a validation failure.
// Key is the field's dotted YAML path (e.g. "csrf.secret") or, for an
// env-override parse error, the variable name.
type Problem struct {
	Key     string
	Message string
}

// LoadError aggregates every [Problem] found during [Load], so a
// misconfigured deployment reports all of its issues at once rather than
// one per run.
type LoadError struct {
	Problems []Problem
}

// Error renders the problems one per line, sorted by key for stable
// output.
func (e *LoadError) Error() string {
	ps := append([]Problem(nil), e.Problems...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Key < ps[j].Key })

	noun := "problems"
	if len(ps) == 1 {
		noun = "problem"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: %d %s:", len(ps), noun)
	for _, p := range ps {
		fmt.Fprintf(&b, "\n  %s: %s", p.Key, p.Message)
	}
	return b.String()
}
