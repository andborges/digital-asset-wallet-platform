package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestAuthMiddleware_RejectsMissingToken(t *testing.T) {
	mw := AuthMiddleware([]string{"secret-token"})
	handler := mw(newOKHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestAuthMiddleware_RejectsInvalidToken(t *testing.T) {
	mw := AuthMiddleware([]string{"secret-token"})
	handler := mw(newOKHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_RejectsMalformedHeader(t *testing.T) {
	mw := AuthMiddleware([]string{"secret-token"})
	handler := mw(newOKHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", nil)
	req.Header.Set("Authorization", "secret-token") // missing "Bearer " prefix
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_AllowsValidToken(t *testing.T) {
	mw := AuthMiddleware([]string{"secret-token", "other-token"})
	handler := mw(newOKHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q — the wrapped handler should have run", rec.Body.String(), "ok")
	}
}

func TestAuthMiddleware_NoValidTokensConfiguredRejectsEverything(t *testing.T) {
	mw := AuthMiddleware(nil)
	handler := mw(newOKHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
