// Command mock-anthropic-backend is a minimal stand-in for Anthropic's
// Messages API, used to verify model-failover's cross-provider fallback
// path end to end: request template conversion (openai -> anthropic),
// response conversion back (anthropic -> openai), a fallback-specific
// path override (e.g. /v1/messages instead of the primary's own
// /chat/completions), and a fallback-specific credential (api-key or
// oauth2) actually overriding whatever the original request carried.
//
// Mirrors mock-model-backend's exact structure/conventions (same debug
// endpoints/semantics) — the only difference is the request/response shape
// this server speaks and validates.
//
// Every request (any method/path) returns an Anthropic Messages API shaped
// response. If the received body doesn't look like a converted Anthropic
// request (no top-level "messages" array, or "max_tokens" missing — a real
// Anthropic API would reject both), this returns 400 instead of the normal
// 200, so a broken template adapter fails loudly in e2e rather than
// silently returning a plausible-looking response.
//
// GET /debug/last-request returns the full headers/body/path of the most
// recent request.
//
// POST /debug/force-status?code=500[&sticky=true][&header=Name:Value] — see
// mock-model-backend's doc comment; identical semantics here.
//
// POST /debug/reset clears the forced status and the last-request record.
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
	forcedStatus  int
	forcedSticky  bool
	forcedHeaderK string
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
	addr := envOr("ADDR", ":9714")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/last-request", handleLastRequest)
	mux.HandleFunc("POST /debug/force-status", handleForceStatus)
	mux.HandleFunc("POST /debug/reset", handleReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", handleAny)

	log.Printf("mock-anthropic-backend listening on %s", addr)
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
	messages, hasMessages := reqBody["messages"].([]interface{})
	_, hasMaxTokens := reqBody["max_tokens"]

	if !hasMessages || len(messages) == 0 || !hasMaxTokens {
		// A real Anthropic Messages API would reject this shape outright —
		// surfacing it as a 400 here means a broken/missing template
		// conversion fails loudly in e2e instead of this mock silently
		// returning a plausible response for a malformed request.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "invalid_request_error", "message": "request is not a valid Anthropic Messages API body (missing messages/max_tokens)"},
		})
		return
	}

	resp := map[string]interface{}{
		"id":    "mock-msg-1",
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []map[string]string{
			{"type": "text", "text": "received model: \"" + model + "\""},
		},
		"stop_reason": "end_turn",
		"usage": map[string]int{
			"input_tokens":  1,
			"output_tokens": 1,
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
