package jobs

import (
	"encoding/json"
	"fmt"
)

// encodePayload serialises a job's struct into the persistent
// payload. JSON is the only format the engine supports; switching
// would require a schema migration and a different `payload` column
// type on the SQL backends.
func encodePayload(job Job) ([]byte, error) {
	if job == nil {
		return nil, fmt.Errorf("encodePayload: nil job")
	}
	return json.Marshal(job)
}

// decodePayload writes the persisted payload into a fresh job
// instance produced by the registered constructor. The caller is
// responsible for handing in a non-nil pointer of the right type;
// the runtime gets this from the registry, never the user.
func decodePayload(data []byte, into Job) error {
	if into == nil {
		return fmt.Errorf("decodePayload: nil destination")
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%w: %v", ErrPayloadDecode, err)
	}
	return nil
}
