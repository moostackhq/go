package sqlx

import (
	"fmt"
	"regexp"
	"strings"
)

// identifierRe matches a SQL identifier that is safe to interpolate
// into a statement. It admits one or more dot-separated segments,
// each either
//
//   - a bare identifier: [A-Za-z_][A-Za-z0-9_$]*, or
//   - a double-quoted identifier: "..." with "" escaping an embedded
//     quote (the SQL-standard quoting that SQLite and PostgreSQL use).
//
// So users, public.users, "order", and myschema."weird name" all
// pass, while anything an injection needs — whitespace, ';', '(',
// '--', a stray quote — is rejected.
var identifierRe = regexp.MustCompile(
	`^(?:[A-Za-z_][A-Za-z0-9_$]*|"(?:[^"]|"")*")` +
		`(?:\.(?:[A-Za-z_][A-Za-z0-9_$]*|"(?:[^"]|"")*"))*$`)

// validIdentifier reports whether s is a syntactically valid SQL
// identifier (optionally schema-qualified and/or double-quoted) and
// therefore safe to interpolate into a statement. Used to guard the
// table name on writes and the column names derived from struct
// fields / db: tags — neither can be a bound placeholder.
func validIdentifier(s string) bool {
	return identifierRe.MatchString(s)
}

// unquoteIdent returns the logical name of a column identifier for
// scan matching. A double-quoted identifier ("...") has its
// surrounding quotes stripped and its "" escapes collapsed to a
// single ", because a driver reports the *unquoted* name in
// rows.Columns() (e.g. a column declared "Order" comes back as
// Order). A bare identifier is returned unchanged.
//
// Only a single fully-quoted token is unquoted; bare or dotted names
// pass through as-is. Dotted/partially-quoted forms don't occur for
// real result columns, so this is sufficient for keying colToField.
func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// checkTable validates a write helper's table argument, returning an
// [ErrInvalidIdentifier]-wrapped error naming the calling function on
// failure. fn is the qualified name used in the message, e.g.
// "sqlx.Insert".
func checkTable(fn, table string) error {
	if !validIdentifier(table) {
		return fmt.Errorf(
			"%w: %s: table %q — table names are interpolated, not "+
				"parameterized, so they must be developer-controlled "+
				"constants, never user input",
			ErrInvalidIdentifier, fn, table)
	}
	return nil
}
