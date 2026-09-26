/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// mock-llm-provider is a scriptable LLM backend for the model-failover
// integration tests. It answers OpenAI Chat Completions (any path ending in
// /chat/completions) and Anthropic Messages (any path ending in /v1/messages),
// streaming or not, and its behaviour is switched at runtime:
//
//	PUT    /__mode      {"mode":"ok"}            normal responses
//	                    {"mode":"status:503"}    that status with an error body
//	                    {"mode":"hang:10"}       wait 10s before responding
//	                    {"mode":"reset"}         close the connection without a response
//	                    {"mode":"stream-abort:2"} send headers + 2 SSE chunks, then close
//	                    {"mode":"seq:status:503,ok"} one mode per request, in order;
//	                                              the last one repeats
//	GET    /__requests  {"count":N,"last":{"path":..,"headers":{..},"body":..}}
//	DELETE /__requests  reset the counter and restore mode "ok"
//	GET    /health      liveness (not counted)
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

type recorded struct {
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type state struct {
	mu    sync.Mutex
	mode  string
	seq   []string
	count int
	last  *recorded
}

var (
	st   = &state{mode: "ok"}
	name = envOr("MOCK_NAME", "mock-llm")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/__mode", handleMode)
	mux.HandleFunc("/__requests", handleRequests)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/", handleLLM)
	addr := ":" + envOr("PORT", "8080")
	log.Printf("%s listening on %s", name, addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "use PUT", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Mode == "" {
		http.Error(w, `body must be {"mode":"..."}`, http.StatusBadRequest)
		return
	}
	st.mu.Lock()
	st.mode, st.seq = req.Mode, nil
	if strings.HasPrefix(req.Mode, "seq:") {
		st.seq = strings.Split(strings.TrimPrefix(req.Mode, "seq:"), ",")
	}
	st.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func handleRequests(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch r.Method {
	case http.MethodDelete:
		st.count, st.last, st.mode, st.seq = 0, nil, "ok", nil
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": st.count, "last": st.last})
	default:
		http.Error(w, "use GET or DELETE", http.StatusMethodNotAllowed)
	}
}

func handleLLM(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	headers := map[string]string{}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	st.mu.Lock()
	st.count++
	st.last = &recorded{Path: r.URL.RequestURI(), Headers: headers, Body: string(body)}
	mode := st.mode
	if len(st.seq) > 0 {
		mode = st.seq[0]
		if len(st.seq) > 1 {
			st.seq = st.seq[1:]
		}
	}
	st.mu.Unlock()

	anthropic := strings.HasSuffix(r.URL.Path, "/v1/messages")
	var parsed struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &parsed)

	switch {
	case strings.HasPrefix(mode, "status:"):
		code, _ := strconv.Atoi(strings.TrimPrefix(mode, "status:"))
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":{"message":"%s scripted %d","type":"mock_error"}}`, name, code)
		return
	case strings.HasPrefix(mode, "hang:"):
		secs, _ := strconv.Atoi(strings.TrimPrefix(mode, "hang:"))
		select {
		case <-time.After(time.Duration(secs) * time.Second):
		case <-r.Context().Done():
			return
		}
	case mode == "reset":
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}

	if strings.HasPrefix(mode, "stream-abort:") {
		n, _ := strconv.Atoi(strings.TrimPrefix(mode, "stream-abort:"))
		streamThenAbort(w, n, anthropic)
		return
	}
	if parsed.Stream {
		stream(w, parsed.Model, anthropic)
		return
	}
	w.Header().Set("content-type", "application/json")
	if anthropic {
		fmt.Fprintf(w, `{"id":"msg_mock","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"hello from %s"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":4}}`, parsed.Model, name)
		return
	}
	fmt.Fprintf(w, `{"id":"chatcmpl-mock","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hello from %s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`, parsed.Model, name)
}

func stream(w http.ResponseWriter, model string, anthropic bool) {
	f, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	send := func(s string) {
		_, _ = io.WriteString(w, s)
		if f != nil {
			f.Flush()
		}
	}
	if anthropic {
		send(fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mock\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n", model))
		send("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		send(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello from %s\"}}\n\n", name))
		send("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		send("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n")
		send("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return
	}
	send(fmt.Sprintf("data: {\"id\":\"chatcmpl-mock\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello from %s\"},\"finish_reason\":null}]}\n\n", model, name))
	send(fmt.Sprintf("data: {\"id\":\"chatcmpl-mock\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", model))
	send("data: [DONE]\n\n")
}

func streamThenAbort(w http.ResponseWriter, chunks int, anthropic bool) {
	f, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for i := 0; i < chunks; i++ {
		if anthropic {
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"part%d \"}}\n\n", i)
		} else {
			fmt.Fprintf(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part%d \"}}]}\n\n", i)
		}
		if f != nil {
			f.Flush()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}
