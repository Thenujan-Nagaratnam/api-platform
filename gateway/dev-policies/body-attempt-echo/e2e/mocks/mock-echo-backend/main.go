// Command mock-echo-backend is a minimal stand-in backend used to manually
// and automatically verify that the body-attempt-echo dev policy actually
// rewrote the outbound request body's "attempt" field on each individual
// upstream retry attempt - not just that the gateway eventually returned a
// clean response.
//
// Unlike mock-ai-backend (which only tracks the single most recent request),
// this server keeps a full ordered HISTORY of every request it receives, so
// a test can distinguish attempt 1's original body from attempt 2's mutated
// one - see GET /debug/history. GET /debug/last-request returns only the
// most recent entry, for parity with the other e2e mocks' convention.
//
// Every request (any method/path) is echoed back as JSON: the exact body it
// received, plus the parsed "attempt" field if the body was JSON - so a
// plain curl through the gateway shows the delivered attempt number
// directly.
//
// POST /debug/force-status?code=503 makes the NEXT request only (any
// method/path) return that status instead of the normal 200 - used to make
// Envoy retry the route (paired with body-attempt-echo's
// x-wso2-retry-conditions statusCodes). Consumed on use: the request after
// that one goes back to the normal 200 behavior.
//
// POST /debug/reset clears the history and any pending forced status, for a
// clean slate between test flows.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	mu           sync.Mutex
	history      []requestRecord
	forcedStatus int // 0 = normal 200 behavior; otherwise, the status to return once, then reset to 0
)

type requestRecord struct {
	Time            time.Time           `json:"time"`
	Method          string              `json:"method"`
	Path            string              `json:"path"`
	Headers         map[string][]string `json:"headers"`
	Body            string              `json:"body"`
	ParsedAttempt   *float64            `json:"parsedAttempt,omitempty"`
	ForcedStatusHit int                 `json:"forcedStatusHit"` // always present (0 = not forced) so consumers never see it as absent/undefined
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("request: method=%s path=%s remote=%s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := envOr("ADDR", ":9720")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/history", handleHistory)
	mux.HandleFunc("GET /debug/last-request", handleLastRequest)
	mux.HandleFunc("POST /debug/force-status", handleForceStatus)
	mux.HandleFunc("POST /debug/reset", handleReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", handleAny) // catch-all: acts as the backend for any path

	log.Printf("mock-echo-backend listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, loggingMiddleware(mux)))
}

func handleAny(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	record := requestRecord{
		Time:    time.Now().UTC(),
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: map[string][]string(r.Header),
		Body:    string(body),
	}

	var decoded map[string]interface{}
	if json.Unmarshal(body, &decoded) == nil {
		if attempt, ok := decoded["attempt"].(float64); ok {
			record.ParsedAttempt = &attempt
		}
	}

	mu.Lock()
	status := forcedStatus
	forcedStatus = 0 // consumed - only the request that triggered this one is forced
	if status != 0 {
		record.ForcedStatusHit = status
	}
	history = append(history, record)
	mu.Unlock()

	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "forced_status_for_test"})
		return
	}

	resp := map[string]interface{}{
		"receivedBody":    string(body),
		"receivedAttempt": record.ParsedAttempt,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(history)
}

func handleLastRequest(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if len(history) == 0 {
		_ = json.NewEncoder(w).Encode(requestRecord{})
		return
	}
	_ = json.NewEncoder(w).Encode(history[len(history)-1])
}

func handleForceStatus(w http.ResponseWriter, r *http.Request) {
	code, err := strconv.Atoi(r.URL.Query().Get("code"))
	if err != nil || code < 100 || code > 599 {
		http.Error(w, "code query parameter must be a valid HTTP status code", http.StatusBadRequest)
		return
	}
	mu.Lock()
	forcedStatus = code
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	forcedStatus = 0
	history = nil
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
