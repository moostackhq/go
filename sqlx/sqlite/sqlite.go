// Package sqlite registers an sqlx [Classifier] for SQLite driver
// errors. Import for the init side effect:
//
//	import _ "github.com/moostackhq/go/sqlx/sqlite"
//
// After this import, errors.Is(err, sqlx.ErrUnique) etc. work on
// errors returned from sqlx helpers that hit a SQLite backend.
package sqlite

import (
	"strings"

	"github.com/moostackhq/go/sqlx"
)

func init() {
	sqlx.RegisterClassifier(classify)
}

func classify(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return sqlx.ErrUnique
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return sqlx.ErrForeignKey
	case strings.Contains(msg, "CHECK constraint failed"):
		return sqlx.ErrCheck
	}
	return nil
}
