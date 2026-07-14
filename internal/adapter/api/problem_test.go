package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteProblem_SetsContentTypeAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteProblem(rec, http.StatusBadRequest, "missing-idempotency-key", "Idempotency-Key header is required", "/v1/customers")

	if got := rec.Result().StatusCode; got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}

	var body ProblemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body did not decode as ProblemDetails: %v", err)
	}
	if body.Status != http.StatusBadRequest {
		t.Fatalf("body.Status = %d, want %d", body.Status, http.StatusBadRequest)
	}
	if body.Title != "missing-idempotency-key" {
		t.Fatalf("body.Title = %q, want %q", body.Title, "missing-idempotency-key")
	}
	if body.Detail == nil || *body.Detail != "Idempotency-Key header is required" {
		t.Fatalf("body.Detail = %v, want %q", body.Detail, "Idempotency-Key header is required")
	}
	if body.Instance == nil || *body.Instance != "/v1/customers" {
		t.Fatalf("body.Instance = %v, want %q", body.Instance, "/v1/customers")
	}
	if body.Type == "" {
		t.Fatal("body.Type must not be empty")
	}
}
