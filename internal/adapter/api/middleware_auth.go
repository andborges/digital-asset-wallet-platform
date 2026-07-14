package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AuthMiddleware requires a valid `Authorization: Bearer <token>` header against a
// static, caller-provided set of valid tokens. There is no anonymous route in this
// service, ever (NFR15, AD-14) — this middleware runs first in the chain, before
// idempotency, so an unauthenticated caller can never probe idempotency-key state.
func AuthMiddleware(validTokens []string) func(http.Handler) http.Handler {
	// Retain the raw list (not a map) so matching can be constant-time per candidate.
	tokens := append([]string(nil), validTokens...)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || !tokenIsValid(tokens, token) {
				WriteProblem(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required", r.URL.Path)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tokenIsValid reports whether presented matches any configured token, using a
// constant-time comparison so a network attacker cannot recover the secret by timing.
// Every configured token is compared (no early return on match) so the number of
// comparisons does not leak which token matched. An empty token list fails closed.
func tokenIsValid(tokens []string, presented string) bool {
	match := 0
	for _, t := range tokens {
		match |= subtle.ConstantTimeCompare([]byte(t), []byte(presented))
	}
	return match == 1
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(header, prefix)
	if token == "" {
		return "", false
	}
	return token, true
}
