package sqlx

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors. Driver classifiers (registered via
// [RegisterClassifier]) wrap raw driver errors with one of these so
// errors.Is(err, ErrUnique) works regardless of backend.
var (
	ErrNotFound   = errors.New("sqlx: not found")
	ErrUnique     = errors.New("sqlx: unique constraint violation")
	ErrForeignKey = errors.New("sqlx: foreign key violation")
	ErrCheck      = errors.New("sqlx: check constraint violation")
)

// ErrInvalidIdentifier is returned by the write helpers when the
// table name is not a syntactically valid SQL identifier. A table
// name is interpolated into the statement — it cannot be a bound
// placeholder — so the library validates it to stop SQL injection
// through an identifier position.
//
// The where expression is deliberately NOT validated: it is an
// arbitrary SQL fragment by design (operators, AND/OR, parentheses,
// ? placeholders). Treat it as a developer-controlled constant and
// bind every value through a ? placeholder; never build it from user
// input. See [Update] and [Delete].
var ErrInvalidIdentifier = errors.New("sqlx: invalid SQL identifier")

// ErrDuplicateColumn is returned when two exported fields of a row
// struct resolve to the same column name — via colliding snake_case
// derivations or `db:` tag overrides. It surfaces from any helper
// that builds a field map for the type (the reads and the writes),
// on first use of that type. This is a struct-definition error: fix
// the field names or `db:` tags.
var ErrDuplicateColumn = errors.New("sqlx: duplicate column")

// ErrUnsupportedFieldType is returned when a row struct has a field
// whose type is itself a struct (embedded or named) that database/sql
// cannot treat as a single column — it is not time.Time and
// implements neither [database/sql/driver.Valuer] nor
// [database/sql.Scanner]. The library does not flatten nested structs
// (see the package doc); such a field would otherwise bind as one
// opaque value on writes and silently read back zero. Flatten it into
// scalar fields, give the type a Valuer + Scanner, or skip it with
// `db:"-"`.
var ErrUnsupportedFieldType = errors.New("sqlx: unsupported struct field type")

// ErrNoColumns is returned by the write helpers when there are no
// columns to write: the row struct has no exported fields, or — for
// [InsertMany] — every row's only field is a zero-value auto-PK that
// gets omitted, leaving nothing to insert per row. Without the guard
// the generated statement is malformed (an empty SET clause or column
// list), producing a cryptic driver error.
//
// [Insert] does NOT return this for the zero-PK case: a single row of
// all defaults is written with the portable INSERT … DEFAULT VALUES
// form. InsertMany has no portable multi-row equivalent, so it
// reports ErrNoColumns instead.
var ErrNoColumns = errors.New("sqlx: struct has no columns to write")

// Classifier inspects a raw driver error and returns one of the
// sentinel errors above (ErrUnique, ErrForeignKey, ErrCheck) when
// the error matches. Returns nil to defer to other classifiers.
type Classifier func(err error) (sentinel error)

var (
	classifiersMu sync.RWMutex
	classifiers   []Classifier
)

// RegisterClassifier registers a driver-specific classifier.
// Sub-packages register their classifier in init():
//
//	// sqlx/sqlite/sqlite.go
//	func init() { sqlx.RegisterClassifier(classify) }
//
// Order: classifiers run in registration order; the first non-nil
// return wins. The library does not deduplicate, so registering the
// same classifier twice runs it twice.
func RegisterClassifier(c Classifier) {
	classifiersMu.Lock()
	defer classifiersMu.Unlock()
	classifiers = append(classifiers, c)
}

// classify is called on every error returned by a database/sql
// operation. It walks the registered classifiers; if any returns a
// sentinel, classify wraps the original error so both
// errors.Is(err, sentinel) AND errors.Is(err, originalDriverError)
// return true.
//
// The classifier slice is copied under the read lock so the lock
// can be released before classifiers run (classifiers are user code
// — they shouldn't be held under the package's lock). The copy is
// explicit (`append([]Classifier(nil), classifiers...)`) rather
// than a bare slice-header copy so the iteration below observes a
// snapshot that does not alias the live slice's backing array.
func classify(err error) error {
	if err == nil {
		return nil
	}
	classifiersMu.RLock()
	cs := append([]Classifier(nil), classifiers...)
	classifiersMu.RUnlock()
	for _, c := range cs {
		if sentinel := c(err); sentinel != nil {
			return fmt.Errorf("%w: %w", sentinel, err)
		}
	}
	return err
}
