package rendermap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// JSONText is the JSON of a value for the YAML editor: whole numbers without an
// exponent, no HTML escaping.
func JSONText(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalizeNumbers(v)); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return json.Number(strconv.FormatInt(int64(x), 10))
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = normalizeNumbers(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = normalizeNumbers(c)
		}
		return out
	}
	return v
}

// ToJSONValue converts a decoded value to what encoding/json decodes: YAML gives
// ints and maps with any key type, JSON float64 and string keys.
func ToJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = ToJSONValue(c)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[fmt.Sprint(k)] = ToJSONValue(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = ToJSONValue(c)
		}
		return out
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return v
}
