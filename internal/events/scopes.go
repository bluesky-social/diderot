package events

import "slices"

func ScopeAllowed(scope string, allowed []string) bool {
	return scope != "" && scope != ScopeUnstamped && slices.Contains(allowed, scope)
}

func ReadableScopes(scopes []string) []string {
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope != "" && scope != ScopeUnstamped {
			result = append(result, scope)
		}
	}
	return result
}
