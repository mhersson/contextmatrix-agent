package tools

import (
	"fmt"
	"strings"
)

func requireString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}

	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}

	return s, nil
}

func optString(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok {
		return v
	}

	return def
}

func optBool(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}

	return false
}

// optInt accepts JSON numbers (decoded as float64) or ints.
func optInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return def
	}
}

// optStringSlice coerces a JSON array (decoded as []any), a []string, or a
// single space-separated string into []string. Missing key -> nil.
func optStringSlice(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}

		return out
	case string:
		if v == "" {
			return nil
		}

		return strings.Fields(v)
	default:
		return nil
	}
}
