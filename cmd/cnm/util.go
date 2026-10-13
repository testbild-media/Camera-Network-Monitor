package main

import "encoding/json"

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func jsonMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
