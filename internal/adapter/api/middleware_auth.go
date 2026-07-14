package api

import (
	"net/http"
	"strings"
)

// AuthMiddleware requires a valid `Authorization: Bearer <token>` header against a
// static, caller-provided set of valid tokens. There is no anonymous route in this
// service, ever (NFR15, AD-14) — this middleware runs first in the chain, before
// idempotency, so an unauthenticated caller can never probe idempotency-key state.
func AuthMiddleware(validTokens []string) func(http.Handler) http.Handler {
	valid := make(map[string]struct{}, len(validTokens))
	for _, t := range validTokens {
		valid[t] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || len(valid) == 0 {
				WriteProblem(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required", r.URL.Path)
				return
			}
			if _, ok := valid[token]; !ok {
				WriteProblem(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required", r.URL.Path)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
