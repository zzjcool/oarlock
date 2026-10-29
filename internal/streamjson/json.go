package streamjson

import "encoding/json"

// Rec is one decoded JSON object. Native frames are read through the helpers
// below so a frame of an unexpected shape reads as "absent", never as a panic
// and never as a guessed zero.
type Rec = map[string]any

// ParseRec decodes one JSON object; nil when raw is not an object.
func ParseRec(raw []byte) Rec {
	var m Rec
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func AsRec(v any) Rec {
	m, _ := v.(map[string]any)
	return m
}

func AsStr(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// str is AsStr without the flag: "" when v is not a string.
func Str(v any) string {
	s, _ := v.(string)
	return s
}

func AsNum(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func AsList(v any) []any {
	l, _ := v.([]any)
	return l
}

// AsRecList keeps the object members of a list, in order.
func AsRecList(v any) []Rec {
	var out []Rec
	for _, e := range AsList(v) {
		if m := AsRec(e); m != nil {
			out = append(out, m)
		}
	}
	return out
}
