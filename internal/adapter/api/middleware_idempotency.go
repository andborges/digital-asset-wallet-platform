package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// maxRequestBodyBytes caps the request body the idempotency layer will buffer and hash.
// Every mutating request is read fully into memory before hashing, so an uncapped body is
// a memory-exhaustion vector; 1 MiB is far larger than any legitimate v1 request.
const maxRequestBodyBytes = 1 << 20

// IdempotencyMiddleware requires an Idempotency-Key header on every mutating request
// it wraps and guarantees exactly-once effects (AD-5, FR23–FR24):
//
//   - non-mutating method (GET/HEAD/OPTIONS/TRACE) -> passed straight through; no key, no transaction
//   - missing key                              -> 400, handler never called
//   - key already stored, same request body    -> stored response replayed verbatim, handler never called
//   - key already stored, different request body -> 409, handler never called
//   - key not stored -> a transaction opens, the handler runs against it via request context; ONLY a
//     successful (2xx) response is persisted and committed — a non-2xx response rolls the transaction
//     back and is returned WITHOUT being stored, so a transient error never poisons the key
//   - a concurrent request wins the insert race -> this request's transaction rolls back and it returns
//     the winner's stored response (or 409 if the winner used a different body)
//
// The transaction is rolled back on every exit path except a successful commit, including a panic in
// the wrapped handler — so a handler panic can never leak the pooled connection.
//
// This is the one place AD-4's "one transaction per state change" promise is made real for every
// later mutating story that reuses this middleware unchanged.
func IdempotencyMiddleware(txBeginner core.TxBeginner, store core.IdempotencyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isMutatingMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				WriteProblem(w, http.StatusBadRequest, "missing-idempotency-key", "Idempotency-Key header is required", r.URL.Path)
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
					WriteProblem(w, http.StatusRequestEntityTooLarge, "request-body-too-large", "request body exceeds the maximum allowed size", r.URL.Path)
					return
				}
				WriteProblem(w, http.StatusBadRequest, "unreadable-body", "request body could not be read", r.URL.Path)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			hash := requestHash(r.Method, r.URL.Path, bodyBytes)

			if entry, ok, err := store.Lookup(r.Context(), key); err != nil {
				WriteProblem(w, http.StatusInternalServerError, "idempotency-lookup-failed", err.Error(), r.URL.Path)
				return
			} else if ok {
				replayOrConflict(w, r, entry, hash)
				return
			}

			ctx, tx, err := txBeginner.Begin(r.Context())
			if err != nil {
				WriteProblem(w, http.StatusInternalServerError, "transaction-begin-failed", err.Error(), r.URL.Path)
				return
			}

			// Roll back on every path that isn't a successful commit — including a panic
			// unwinding through this defer, which is what returns the pooled connection and
			// prevents a handler panic from leaking it. A detached context is used so the
			// rollback still runs even if the client disconnected and cancelled r.Context().
			committed := false
			defer func() {
				if !committed {
					_ = tx.Rollback(context.WithoutCancel(ctx))
				}
			}()

			rec := newResponseRecorder()
			next.ServeHTTP(rec, r.WithContext(ctx))
			captured := rec.result()

			// A handler that wrote nothing is a programming error; do not commit an
			// empty transaction or cache a synthetic 200.
			if !rec.wroteHeader {
				WriteProblem(w, http.StatusInternalServerError, "empty-handler-response", "the handler produced no response", r.URL.Path)
				return
			}

			// Only persist and commit successful outcomes. Non-2xx responses roll back
			// (via the defer) and pass through unstored, so a retry gets a fresh attempt.
			if captured.Status < 200 || captured.Status >= 300 {
				replay(w, captured)
				return
			}

			insertErr := store.Insert(ctx, key, hash, captured)
			if errors.Is(insertErr, core.ErrKeyConflict) {
				// A concurrent request committed the same key first. Return its stored
				// result — or 409 if that winner used a different body. Use a detached
				// context so the read still succeeds if the client has since disconnected.
				entry, ok, lookupErr := store.Lookup(context.WithoutCancel(r.Context()), key)
				if lookupErr != nil || !ok {
					WriteProblem(w, http.StatusInternalServerError, "idempotency-conflict-unresolved", "a concurrent request won the race but its result could not be read back", r.URL.Path)
					return
				}
				replayOrConflict(w, r, entry, hash)
				return
			}
			if insertErr != nil {
				WriteProblem(w, http.StatusInternalServerError, "idempotency-insert-failed", insertErr.Error(), r.URL.Path)
				return
			}

			if err := tx.Commit(context.WithoutCancel(ctx)); err != nil {
				WriteProblem(w, http.StatusInternalServerError, "transaction-commit-failed", err.Error(), r.URL.Path)
				return
			}
			committed = true

			replay(w, captured)
		})
	}
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// replayOrConflict serves a stored idempotency entry: the original response if the
// request body matches, or 409 if the same key was first used with a different body.
// Used by both the pre-transaction lookup and the concurrent-conflict re-lookup so the
// two paths cannot diverge.
func replayOrConflict(w http.ResponseWriter, r *http.Request, entry core.StoredEntry, hash []byte) {
	if !bytes.Equal(entry.RequestHash, hash) {
		WriteProblem(w, http.StatusConflict, "idempotency-key-reused", "this Idempotency-Key was already used with a different request body", r.URL.Path)
		return
	}
	replay(w, entry.Response)
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
