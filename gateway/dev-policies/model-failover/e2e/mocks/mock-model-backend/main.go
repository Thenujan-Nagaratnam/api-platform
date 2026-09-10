// Command mock-model-backend is a minimal stand-in for an OpenAI-shaped LLM
// backend, used to verify the model-failover gateway policy's response-path
// retry mechanism: that it rewrites the outbound request body's "model"
// field per attempt, dials the correct target, and swaps credentials when
// configured to.
//
// Multiple instances of this same binary run side-by-side on different
// ports (ADDR), one per model-failover target/fallback, mirroring
// dev-policies/oauth2-generator/e2e/mocks/mock-ai-backend's exact
// structure/conventions.
//
// Every request (any method/path) returns an OpenAI chat-completions-shaped
// response so a plain curl (or Postman) through the gateway can see which
// backend and which rewritten model name actually landed here - the
// assistant message content embeds the "model" field this server received,
// followed by the raw request body byte-for-byte. Echoing the raw body back
// (rather than just the model) lets a policy that rewrites the request body
// - e.g. pii-masking-regex replacing PII with a placeholder - be observed
// round-tripping through the response too: whatever this server received
// verbatim is what a caller sees echoed in choices[0].message.content,
// placeholders included, so response-side restoration can be exercised
// end-to-end without a real upstream LLM.
//
// GET /debug/last-request returns the full headers/body/path of the most
// recent request, for scripted assertions (e.g. confirming which credential
// header arrived, or that a fallback's path override took effect).
//
// POST /debug/force-status?code=500 makes the NEXT request only (any
// method/path) return that status instead of the normal 200 - used to
// simulate this target failing, so a caller can observe model-failover's
// retry landing on a different target/model. Add &sticky=true to keep
// returning that status until the next /debug/reset instead of consuming it
// after one request - needed to simulate a target that's down for the whole
// duration of a fallback chain being exhausted, not just its first attempt.
// Add &header=Name:Value to also set one extra response header on that same
// forced response (any code, including 2xx) - used to prove hop-by-hop
// response headers set by a backend are stripped before reaching the client.
//
// POST /debug/reset clears the forced status and the last-request record,
// for a clean slate between test flows.
package main

import (
	"encoding/json"
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
	forcedStatus  int    // 0 = normal 200 behavior; otherwise, the status to return
	forcedSticky  bool   // if true, forcedStatus is NOT cleared after being served
	forcedHeaderK string // optional extra response header to set alongside forcedStatus
	forcedHeaderV string
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

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("request: method=%s path=%s remote=%s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := envOr("ADDR", ":9711")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/last-request", handleLastRequest)
	mux.HandleFunc("POST /debug/force-status", handleForceStatus)
	mux.HandleFunc("POST /debug/reset", handleReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", handleAny) // catch-all: acts as the LLM backend for any path

	log.Printf("mock-model-backend listening on %s", addr)
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

	mu.Lock()
	lastRequest = record
	status := forcedStatus
	headerK, headerV := forcedHeaderK, forcedHeaderV
	if !forcedSticky {
		forcedStatus = 0
		forcedHeaderK, forcedHeaderV = "", ""
	}
	mu.Unlock()

	if headerK != "" {
		w.Header().Set(headerK, headerV)
	}

	if status != 0 && status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "forced_status_for_test"})
		return
	}

	var reqBody map[string]interface{}
	_ = json.Unmarshal(body, &reqBody)
	model, _ := reqBody["model"].(string)

	resp := map[string]interface{}{
		"id":      "mock-chatcmpl-1",
		"object":  "chat.completion",
		"model":   model,
		"created": time.Now().Unix(),
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": "received model: \"" + model + "\", body: " + string(body),
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

	// status == 0 (no force) or status == http.StatusOK (a forced 200, used
	// only to attach an extra response header for the hop-by-hop test) both
	// fall through to this normal successful body. Content-Type must be set
	// before WriteHeader — setting it after is silently ignored by net/http.
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
	sticky := r.URL.Query().Get("sticky") == "true"

	var headerK, headerV string
	if h := r.URL.Query().Get("header"); h != "" {
		if name, value, ok := strings.Cut(h, ":"); ok {
			headerK, headerV = name, value
		}
	}

	mu.Lock()
	forcedStatus = code
	forcedSticky = sticky
	forcedHeaderK, forcedHeaderV = headerK, headerV
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	forcedStatus = 0
	forcedSticky = false
	forcedHeaderK, forcedHeaderV = "", ""
	lastRequest = lastRequestRecord{}
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
