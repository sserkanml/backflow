package managedfields

import "encoding/json"

// jsonOf encodes a decoded value (a map of the fieldsV1 object) as JSON.
func jsonOf(v any) ([]byte, error) { return json.Marshal(v) }
