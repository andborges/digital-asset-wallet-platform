package api

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// IdempotencyMiddleware requires an Idempotency-Key header on every mutating request
// it wraps and guarantees exactly-once effects (AD-5, FR23–FR24):
//
//   - missing key                              -> 400, handler never called
//   - key already stored, same request body    -> stored response replayed verbatim, handler never called
//   - key already stored, different request body -> 409, handler never called
//   - key not stored                            -> a transaction opens, the handler runs against it via
//     request context, its response is captured, the idempotency row is inserted in the SAME transaction,
//     then the transaction commits and only THEN is the captured response flushed to the real client
//   - a concurrent request wins the race to insert first -> this request's transaction rolls back and
//     it returns the winner's stored response instead of a 500
//
// This is the one place AD-4's "one transaction per state change" promise is made real for every
// later mutating story that reuses this middleware unchanged.
func IdempotencyMiddleware(txBeginner core.TxBeginner, store core.IdempotencyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				WriteProblem(w, http.StatusBadRequest, "missing-idempotency-key", "Idempotency-Key header is required", r.URL.Path)
				return
			}

			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				WriteProblem(w, http.StatusBadRequest, "unreadable-body", "request body could not be read", r.URL.Path)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			hash := requestHash(r.Method, r.URL.Path, bodyBytes)

			if entry, ok, err := store.Lookup(r.Context(), key); err != nil {
				WriteProblem(w, http.StatusInternalServerError, "idempotency-lookup-failed", err.Error(), r.URL.Path)
				return
			} else if ok {
				if !bytes.Equal(entry.RequestHash, hash) {
					WriteProblem(w, http.StatusConflict, "idempotency-key-reused", "this Idempotency-Key was already used with a different request body", r.URL.Path)
					return
				}
				replay(w, entry.Response)
				return
			}

			ctx, tx, err := txBeginner.Begin(r.Context())
			if err != nil {
				WriteProblem(w, http.StatusInternalServerError, "transaction-begin-failed", err.Error(), r.URL.Path)
				return
			}

			rec := newResponseRecorder()
			next.ServeHTTP(rec, r.WithContext(ctx))
			captured := rec.result()

			insertErr := store.Insert(ctx, key, hash, captured)
			if errors.Is(insertErr, core.ErrKeyConflict) {
				_ = tx.Rollback(ctx)
				entry, ok, lookupErr := store.Lookup(r.Context(), key)
				if lookupErr != nil || !ok {
					WriteProblem(w, http.StatusInternalServerError, "idempotency-conflict-unresolved", "a concurrent request won the race but its result could not be read back", r.URL.Path)
					return
				}
				replay(w, entry.Response)
				return
			}
			if insertErr != nil {
				_ = tx.Rollback(ctx)
				WriteProblem(w, http.StatusInternalServerError, "idempotency-insert-failed", insertErr.Error(), r.URL.Path)
				return
			}

			if err := tx.Commit(ctx); err != nil {
				WriteProblem(w, http.StatusInternalServerError, "transaction-commit-failed", err.Error(), r.URL.Path)
				return
			}

			replay(w, captured)
		})
	}
}

func requestHash(method, path string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte("\n"))
	h.Write([]byte(path))
	h.Write([]byte("\n"))
	h.Write(body)
	return h.Sum(nil)
}

// replay writes a previously captured (or just-captured) response to the real client,
// byte-for-byte (AC2) — this is the only place a StoredResponse becomes an actual HTTP response.
func replay(w http.ResponseWriter, resp core.StoredResponse) {
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// responseRecorder buffers a handler's response instead of writing it to the real
// client immediately, so IdempotencyMiddleware can decide whether to commit before
// anything is visible outside the transaction.
type responseRecorder struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header)}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(b)
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
}

func (r *responseRecorder) result() core.StoredResponse {
	status := r.status
	if !r.wroteHeader {
		status = http.StatusOK
	}
	return core.StoredResponse{
		Status:      status,
		Body:        append([]byte(nil), r.body.Bytes()...),
		ContentType: r.header.Get("Content-Type"),
	}
}
