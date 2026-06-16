package sqlx

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSON wraps any Go value as a JSON-encoded column. It implements
// [database/sql.Scanner] and [database/sql/driver.Valuer], so the
// underlying column type can be TEXT (sqlite), JSONB (postgres),
// or any string-shaped column — the wrapper marshals on insert and
// unmarshals on scan transparently.
//
//	type Event struct {
//	    ID       int64
//	    Metadata sqlx.JSON[map[string]any]
//	}
//
// On NULL columns, Scan resets V to its zero value.
type JSON[T any] struct {
	V T
}

// Scan implements [database/sql.Scanner].
func (j *JSON[T]) Scan(src any) error {
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	case nil:
		var zero T
		j.V = zero
		return nil
	default:
		return fmt.Errorf("sqlx.JSON: cannot scan %T", src)
	}
	return json.Unmarshal(data, &j.V)
}

// Value implements [database/sql/driver.Valuer]. Returned bytes are
// the JSON-encoded form of V.
func (j JSON[T]) Value() (driver.Value, error) {
	return json.Marshal(j.V)
}
