package template

import (
	"fmt"
	"time"
)

// DefaultFuncs returns the library's generic template helpers.
// [Load] merges this map into the user-provided funcs (user wins on
// collision), so every parsed template has access to:
//
//   - add(a, b int) int                  — sum
//   - sub(a, b int) int                  — difference
//   - slice(s string, i int) string      — s[i:], empty when i is out of range
//   - humanizeAge(t time.Time) string    — short "5s" / "12m" / "3h"
//
// Call directly if you want to merge with multiple FuncMaps before
// passing to Load.
func DefaultFuncs() FuncMap {
	return FuncMap{
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
		"slice":       sliceFrom,
		"humanizeAge": humanizeAge,
	}
}

func sliceFrom(s string, start int) string {
	if start < 0 || start > len(s) {
		return ""
	}
	return s[start:]
}

func humanizeAge(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
