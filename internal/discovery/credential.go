package discovery

import (
	"crypto/subtle"
	"strings"
)

// bearerMatches reports whether an Authorization header carries token.
//
// The comparison is constant time and an empty configured token never matches,
// so a deployment that forgot to set one cannot accept anonymous registrations.
func bearerMatches(header, token string) bool {
	token = strings.TrimSpace(token)
	value := strings.TrimSpace(header)
	if len(value) >= len("bearer ") && strings.EqualFold(value[:len("bearer ")], "bearer ") {
		value = strings.TrimSpace(value[len("bearer "):])
	}
	if token == "" || value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1
}
