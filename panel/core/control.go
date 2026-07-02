package core

import "strings"

// isAuthError returns true if err represents a 401 Unauthorized response
// from the control API (wrong/mismatched token).
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized")
}
