package llm

import (
	"encoding/json"
	"math"
	"strings"
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

func inferContextLimitFromModelName(model string) int {
	lower := strings.ToLower(model)

	for _, suffix := range []struct {
		token string
		limit int
	}{
		{"128k", 128000},
		{"256k", 256000},
		{"200k", 200000},
		{"1m", 1000000},
		{"32k", 32768},
		{"16k", 16385},
		{"8k", 8192},
		{"4k", 4096},
	} {
		if strings.Contains(lower, suffix.token) {
			return suffix.limit
		}
	}

	switch {
	case strings.HasPrefix(lower, "o1"),
		strings.HasPrefix(lower, "o3"),
		strings.HasPrefix(lower, "o4"):
		return 200000
	case strings.HasPrefix(lower, "deepseek-v4"):
		return 1000000
	case strings.Contains(lower, "gpt-4o"),
		strings.Contains(lower, "gpt-4.1"),
		strings.Contains(lower, "gpt-4-turbo"),
		strings.Contains(lower, "gpt-4.5"):
		return 128000
	case strings.Contains(lower, "gpt-4"):
		return 8192
	case strings.Contains(lower, "gpt-3.5-turbo"):
		return 16385
	case strings.Contains(lower, "deepseek-chat"),
		strings.Contains(lower, "deepseek-coder"),
		strings.Contains(lower, "deepseek-reasoner"):
		return 128000
	case strings.Contains(lower, "deepseek"):
		return 128000
	case strings.Contains(lower, "gemini-2.5-pro"):
		return 1048576
	case strings.Contains(lower, "gemini-2.5-flash"):
		return 1048576
	case strings.Contains(lower, "gemini-2.0-flash"):
		return 1048576
	case strings.Contains(lower, "gemini-1.5-pro"):
		return 2097152
	case strings.Contains(lower, "gemini-1.5-flash"):
		return 1048576
	case strings.Contains(lower, "gemini"):
		return 32768
	case strings.Contains(lower, "claude-3.5"),
		strings.Contains(lower, "claude-3-5"):
		return 200000
	case strings.Contains(lower, "claude-3-opus"):
		return 200000
	case strings.Contains(lower, "claude"):
		return 200000
	default:
		return 0
	}
}
