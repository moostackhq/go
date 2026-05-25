package session

import "encoding/json"

// JSONCodec encodes a session payload as JSON. It is the default
// codec because the bytes are inspectable with standard tooling
// (psql, sqlite3 CLI + jq, log scrubbers) and because additive
// struct changes round-trip cleanly without ceremony. The costs are
// time.Time precision (nanos → RFC3339) and the lack of native
// binary support — both fixable with json.Marshaler on user types
// if they matter.
type JSONCodec[T any] struct{}

func (JSONCodec[T]) Encode(v T) ([]byte, error) {
	return json.Marshal(v)
}

func (JSONCodec[T]) Decode(b []byte) (T, error) {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}
