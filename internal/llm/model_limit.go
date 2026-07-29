package llm

import (
	"encoding/json"
	"math"
)

var contextFields = []string{
	"n_ctx",
	"context_window",
	"context_length",
	"max_context_tokens",
	"max_model_len",
	"max_sequence_length",
}

func parseContextLimitFromJSON(raw string) int {
	if raw == "" {
		return 0
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return 0
	}
	if limit := contextLimitFromMap(doc); limit > 0 {
		return limit
	}
	if meta, ok := doc["meta"].(map[string]interface{}); ok {
		if limit := contextLimitFromMap(meta); limit > 0 {
			return limit
		}
	}
	return 0
}

func contextLimitFromMap(doc map[string]interface{}) int {
	for _, key := range contextFields {
		if limit := jsonNumberToInt(doc[key]); limit > 0 {
			return limit
		}
	}
	return 0
}

func jsonNumberToInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		if n <= 0 || n > float64(math.MaxInt) {
			return 0
		}
		return int(n)
	case int:
		if n <= 0 {
			return 0
		}
		return n
	case int64:
		if n <= 0 || n > int64(math.MaxInt) {
			return 0
		}
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil || i <= 0 || i > int64(math.MaxInt) {
			return 0
		}
		return int(i)
	default:
		return 0
	}
}
