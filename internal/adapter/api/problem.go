package api

import (
	"encoding/json"
	"net/http"
)

// ProblemDetails (the RFC 9457 application/problem+json shape) is generated from
// api/openapi.yaml's schema (server.gen.go) — the spec is the single source of truth
// for this shape, never hand-duplicated. Never include a key handle or secret in Detail
// (an NFR13 concern whose shape is set here).

const problemTypeBase = "https://digital-asset-wallet-platform.dev/problems/"

// WriteProblem writes an RFC 9457 problem+json response. title is a short,
// machine-readable slug (also used to derive Type); detail is a human-readable
// explanation; instance identifies the specific request (typically the request path).
func WriteProblem(w http.ResponseWriter, status int, title, detail, instance string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)

	body := ProblemDetails{
		Type:     problemTypeBase + title,
		Title:    title,
		Status:   status,
		Detail:   &detail,
		Instance: &instance,
	}
	_ = json.NewEncoder(w).Encode(body)
}
