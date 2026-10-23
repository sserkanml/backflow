package rendermap

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// RenderValue returns the string form a template prints for a decoded value:
// 3, 3.0 and "3" all render as 3, true as true. Maps, lists and nil have no
// scalar form and render as the empty string.
func RenderValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		if f, err := x.Float64(); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprint(x)
	}
	return ""
}
