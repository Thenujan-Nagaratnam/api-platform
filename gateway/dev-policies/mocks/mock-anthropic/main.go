// mock-anthropic is a standalone, in-memory mock of an Anthropic Messages
// endpoint, for live-verifying the LLM model-failover mechanism (see
// gateway/dev-policies/FAILOVER_TESTING.md). Not part of any Go module in
// the workspace — run with `GOWORK=off go run .`.
package main

import (
	"bufio"
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
	addr := flag.String("addr", ":9612", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/messages", handleMessages)
	mux.HandleFunc("/control/arm-failure", handleArmFailure)
	mux.HandleFunc("/control/reset", handleReset)
	mux.HandleFunc("/control/history", handleHistory)

	log.Printf("mock-anthropic listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload map[string]interface{}
	_ = json.Unmarshal(body, &payload)
	model, _ := payload["model"].(string)
	stream, _ := payload["stream"].(bool)
	auth := r.Header.Get("X-Api-Key")
	if auth == "" {
		auth = r.Header.Get("Authorization")
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

	w.Header().Set("X-Mock-Backend", "anthropic")
	w.Header().Set("X-Mock-Received-Auth", auth)
	w.Header().Set("X-Mock-Received-Model", model)

	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"type": "mock_error", "message": "mock-anthropic armed failure"},
		})
		return
	}

	if stream {
		writeSSEResponse(w, model, n)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":            fmt.Sprintf("msg_mock_anthropic_%d", n),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]interface{}{{"type": "text", "text": "mock-anthropic-response"}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]interface{}{"input_tokens": 10, "output_tokens": 4},
	})
}

// writeSSEResponse emits a small, valid Anthropic Messages event stream, for
// exercising the transformer's buffered-SSE-to-OpenAI-chunks translation path.
func writeSSEResponse(w http.ResponseWriter, model string, n int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriter(w)

	events := []struct {
		event string
		data  map[string]interface{}
	}{
		{"message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id": fmt.Sprintf("msg_mock_anthropic_%d", n), "type": "message", "role": "assistant",
				"model": model, "content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]interface{}{"input_tokens": 10, "output_tokens": 1},
			},
		}},
		{"content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		}},
		{"content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": "mock-anthropic-"},
		}},
		{"content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": "stream-response"},
		}},
		{"content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0}},
		{"message_delta", map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]interface{}{"output_tokens": 5},
		}},
		{"message_stop", map[string]interface{}{"type": "message_stop"}},
	}

	for _, e := range events {
		data, _ := json.Marshal(e.data)
		fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", e.event, data)
		_ = bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// handleArmFailure arms the next N calls to /v1/messages to return the given
// status instead of a normal 200. Body: {"count": 1, "status": 500}.
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
