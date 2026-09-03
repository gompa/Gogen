package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func stringArg(args map[string]any, key string) (string, error) {
	if _, ok := args[key]; !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	return stringArgOptional(args, key)
}
func stringArgOptional(args map[string]any, key string) (string, error) {
	val, ok := args[key]
	if !ok {
		return "", nil
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}
func (a *Agent) toolContext(ctx context.Context) context.Context {
	if a.Executor != nil && !a.Executor.DeleteApprovalRequired() {
		ctx = ContextWithDeleteApprovalRequired(ctx, false)
	}
	return ctx
}

// boolArg reads an optional boolean tool argument, returning def when the
// key is absent.
func boolArg(args map[string]any, key string, def bool) (bool, error) {
	val, ok := args[key]
	if !ok {
		return def, nil
	}
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("argument %q must be a boolean", key)
	}
	return b, nil
}
func intArgOptional(args map[string]any, key string) (int, error) {
	val, ok := args[key]
	if !ok {
		return 0, nil
	}
	switch v := val.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("argument %q must be an integer", key)
		}
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case string:
		// Models sometimes quote numeric arguments (e.g. "id": "3").
		// Coerce a string that parses as a plain integer; anything else
		// (fractions, exponents, non-numeric text) still errors rather
		// than silently becoming 0.
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, fmt.Errorf("argument %q must be an integer", key)
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("argument %q must be an integer", key)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("argument %q must be an integer", key)
	}
}

// intRequiredArg reads a required positive-integer tool argument, e.g. an
// item id. Unlike intArgOptional it errors when the key is absent or the
// value is not a positive integer (ids start at 1, so 0 is invalid). It
// accepts the same value shapes intArgOptional does: JSON numbers
// (float64), int/int64, and quoted numeric strings.
func intRequiredArg(args map[string]any, key string) (int, error) {
	if _, ok := args[key]; !ok {
		return 0, fmt.Errorf("missing required argument %q", key)
	}
	n, err := intArgOptional(args, key)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("argument %q must be a positive integer", key)
	}
	return n, nil
}
func stringSliceArg(args map[string]any, key string) ([]string, error) {
	if _, ok := args[key]; !ok {
		return nil, fmt.Errorf("missing required argument %q", key)
	}
	return stringSliceArgOptional(args, key)
}
func stringSliceArgOptional(args map[string]any, key string) ([]string, error) {
	val, ok := args[key]
	if !ok {
		return nil, nil
	}
	return coerceStringSlice(key, val)
}
func coerceStringSlice(key string, val any) ([]string, error) {
	switch v := val.(type) {
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("argument %q[%d] must be a string", key, i)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("argument %q must be an array of strings", key)
	}
}
