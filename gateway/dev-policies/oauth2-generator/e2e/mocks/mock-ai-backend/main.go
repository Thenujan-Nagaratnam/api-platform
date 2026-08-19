// Command mock-ai-backend is a minimal stand-in for an LLM backend, used to
// manually verify that the oauth2 gateway policy actually injects the
// Authorization header on the outbound request that reaches the upstream —
// not just that the gateway returns 2xx to the caller.
//
// Every request (any method/path) returns an OpenAI chat-completions-shaped
// response so the gateway's "openai" LLM provider template can extract
// prompt/completion/total tokens without erroring. The assistant message
// content embeds the Authorization header this server actually received,
// so a plain curl to /chat/completions through the gateway is enough to see
// the injected bearer token with your own eyes.
//
// GET /debug/last-request returns the full headers/body of the most recent
// request, for scripted assertions (e.g. `curl .../debug/last-request | jq
// .headers.Authorization`).
//
// POST /debug/force-status?code=401 makes the NEXT request only (any
// method/path) return that status instead of the normal 200 - used to
// simulate the upstream rejecting an already-cached bearer token (e.g. it
// was revoked out-of-band), to test the oauth2 policy's
// tokenPurgeStatusCodes. Consumed on use: the request after that
// one goes back to the normal 200 behavior.
//
// POST /debug/revoke-token, body = the exact Authorization header value
// (e.g. "Bearer mock-token-1-issued-..."), marks that SPECIFIC credential as
// permanently rejected: every future request presenting it gets 401,
// indefinitely - not a one-shot flag like /debug/force-status. Any other
// (e.g. freshly issued) token is unaffected. Used to prove a retry actually
// fetched a new credential rather than just resending the one that was just
// rejected.
//
// POST /debug/reset clears the forced status, the last-request record, and
// all revoked tokens, for a clean slate between test flows.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	mu            sync.Mutex
	lastRequest   lastRequestRecord
	forcedStatus  int             // 0 = normal 200 behavior; otherwise, the status to return once, then reset to 0
	revokedTokens map[string]bool // keyed by the exact Authorization header value; never consumed - only /debug/reset clears it
)

type lastRequestRecord struct {
	Time    time.Time           `json:"time"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// maskToken keeps only enough of a bearer token/header to correlate log lines
// without leaking the credential itself (see GO-AUTH-003).
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "[MASKED]"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

// loggingMiddleware logs every inbound request (method, path, remote addr,
// masked Authorization header) so a manual test run has a full audit trail
// of what actually reached the mock backend.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("request: method=%s path=%s remote=%s authorization=%s",
			r.Method, r.URL.Path, r.RemoteAddr, maskToken(r.Header.Get("Authorization")))
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := envOr("ADDR", ":9602")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/last-request", handleLastRequest)
	mux.HandleFunc("POST /debug/force-status", handleForceStatus)
	mux.HandleFunc("POST /debug/revoke-token", handleRevokeToken)
	mux.HandleFunc("POST /debug/reset", handleReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", handleAny) // catch-all: acts as the LLM backend for any path

	log.Printf("mock-ai-backend listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, loggingMiddleware(mux)))
}

func handleAny(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	record := lastRequestRecord{
		Time:    time.Now().UTC(),
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: map[string][]string(r.Header),
		Body:    string(body),
	}
	authHeader := r.Header.Get("Authorization")

	mu.Lock()
	lastRequest = record
	status := forcedStatus
	forcedStatus = 0 // consumed - only the request that triggered this one is forced
	revoked := authHeader != "" && revokedTokens[authHeader]
	mu.Unlock()

	// Checked ahead of forcedStatus, and never consumed: a revoked token must
	// keep failing on every subsequent request, unlike the one-shot flag
	// below - that's the whole point of the distinction (see package doc).
	if revoked {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "revoked_token"})
		return
	}

	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "forced_status_for_test"})
		return
	}

	content := fmt.Sprintf("received Authorization: %q", authHeader)

	resp := map[string]interface{}{
		"id":      "mock-chatcmpl-1",
		"object":  "chat.completion",
		"model":   "mock-model",
		"created": time.Now().Unix(),
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func handleLastRequest(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(lastRequest)
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

// handleRevokeToken marks the exact Authorization header value in the
// request body as permanently rejected - see the package doc for why this is
// deliberately not a one-shot flag like handleForceStatus.
func handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	authHeader := strings.TrimSpace(string(body))
	if authHeader == "" {
		http.Error(w, "request body must be the exact Authorization header value to revoke", http.StatusBadRequest)
		return
	}

	mu.Lock()
	if revokedTokens == nil {
		revokedTokens = make(map[string]bool)
	}
	revokedTokens[authHeader] = true
	mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	forcedStatus = 0
	lastRequest = lastRequestRecord{}
	revokedTokens = nil
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
