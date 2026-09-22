// mock-openai is a standalone, in-memory mock of an OpenAI-compatible chat
// completions endpoint, for live-verifying the LLM model-failover mechanism
// (see gateway/dev-policies/FAILOVER_TESTING.md). Not part of any Go module
// in the workspace — run with `GOWORK=off go run .`.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

var (
	mu                sync.Mutex
	failuresRemaining int
	failStatus        int
	history           []requestRecord
	callCount         int
)

type requestRecord struct {
	Time     string `json:"time"`
	Model    string `json:"model"`
	Auth     string `json:"authorization"`
	Body     string `json:"body"`
	RespCode int    `json:"respCode"`
}

func main() {
	addr := flag.String("addr", ":9611", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/control/arm-failure", handleArmFailure)
	mux.HandleFunc("/control/reset", handleReset)
	mux.HandleFunc("/control/history", handleHistory)

	log.Printf("mock-openai listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload map[string]interface{}
	_ = json.Unmarshal(body, &payload)
	model, _ := payload["model"].(string)
	// Checked in the same order as mock-anthropic: a request can land here via
	// upstreamDefinition while carrying a DIFFERENT provider's credential
	// convention (e.g. X-Api-Key), so this must not only look at Authorization.
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = r.Header.Get("X-Api-Key")
	}

	mu.Lock()
	callCount++
	n := callCount
	status := http.StatusOK
	if failuresRemaining > 0 {
		failuresRemaining--
		status = failStatus
	}
	history = append(history, requestRecord{
		Time: time.Now().Format(time.RFC3339Nano), Model: model, Auth: auth, Body: string(body), RespCode: status,
	})
	mu.Unlock()

	w.Header().Set("X-Mock-Backend", "openai")
	w.Header().Set("X-Mock-Received-Auth", auth)
	w.Header().Set("X-Mock-Received-Model", model)
	w.Header().Set("Content-Type", "application/json")

	if status != http.StatusOK {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{"message": "mock-openai armed failure", "type": "mock_error", "code": fmt.Sprintf("%d", status)},
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-mock-openai-%d", n),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": "mock-openai-response"},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
}

// handleArmFailure arms the next N calls to /v1/chat/completions to return
// the given status instead of a normal 200. Body: {"count": 2, "status": 500}.
func handleArmFailure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Count  int `json:"count"`
		Status int `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Status == 0 {
		req.Status = http.StatusInternalServerError
	}
	mu.Lock()
	failuresRemaining = req.Count
	failStatus = req.Status
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func handleReset(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	failuresRemaining = 0
	failStatus = 0
	history = nil
	callCount = 0
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func handleHistory(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"callCount": callCount,
		"history":   history,
	})
}
