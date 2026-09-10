// Command mock-forward-proxy is a minimal HTTP forward proxy used to test
// the oauth2 gateway policy's proxyURL parameter end to end. It is
// intentionally NOT a general-purpose proxy — it implements exactly the two
// request shapes Go's net/http.Transport sends through a configured proxy:
// an absolute-URI request for a plain HTTP target (what the policy's
// token-endpoint call produces, since mock-oauth2-idp is HTTP-only), and
// CONNECT tunneling for an HTTPS target (kept for completeness, in case a
// test ever points proxyURL at an HTTPS token endpoint).
//
// The point of this mock is to answer one question rigorously: did the
// token-endpoint call actually go through the configured proxy, or did it
// reach the identity provider directly (silently ignoring proxyURL)? GET
// /debug/stats is the answer — if proxyURL took effect, this mock's history
// shows the proxied request; if it didn't, mock-oauth2-idp's own
// /debug/stats shows the request arrived anyway, but this mock's stays
// empty. Comparing the two is the test.
//
// Endpoints:
//
//	(forward proxy)     any absolute-URI HTTP request, or CONNECT for HTTPS -
//	                     proxied to the real target and the response (or
//	                     tunnel) relayed back untouched.
//	GET  /debug/stats    JSON summary of every request this mock has proxied
//	                     so far - target host, method, path, time.
//	POST /debug/reset    Clears the request history (call between test flows).
//	GET  /healthz        Liveness probe. Only reachable as a direct (non-
//	                     proxied) request, since it's not an absolute URI.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	history []proxiedRequestRecord
)

// proxiedRequestRecord captures one request this mock actually proxied -
// GET /debug/stats is how a test proves proxyURL took effect, rather than
// the token-endpoint call reaching the identity provider directly.
type proxiedRequestRecord struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"` // "CONNECT" for an HTTPS tunnel, otherwise the real HTTP method
	Target string    `json:"target"` // host:port (CONNECT) or full absolute URI (HTTP)
}

func recordProxied(method, target string) {
	mu.Lock()
	defer mu.Unlock()
	history = append(history, proxiedRequestRecord{Time: time.Now().UTC(), Method: method, Target: target})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	addr := envOr("ADDR", ":9603")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/stats", handleStats)
	mux.HandleFunc("POST /debug/reset", handleReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Everything else is treated as a proxy request (absolute-URI HTTP) -
	// registered last so /debug/* and /healthz above take precedence for
	// direct (non-proxied) requests to this mock itself.
	mux.HandleFunc("/", handleForwardHTTP)

	// CONNECT requests can't reach the mux above: http.ServeMux matches
	// patterns against r.URL.Path, but a CONNECT request has an empty Path
	// (its target is in r.Host) - "/" never matches, so ServeMux itself
	// returns 404 before handleConnect ever runs. Intercepting the method
	// here, before mux dispatch, is what actually makes HTTPS proxying work.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	log.Printf("mock-forward-proxy listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}

// handleForwardHTTP re-issues an absolute-URI HTTP request to its real
// target and relays the response back untouched - this is the path the
// oauth2 policy's token-endpoint call takes when proxyURL is set and the
// token endpoint is plain HTTP (as mock-oauth2-idp is).
func handleForwardHTTP(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "not a proxy request (missing absolute URI) - point proxyURL at this mock, not the other way around", http.StatusBadRequest)
		return
	}

	recordProxied(r.Method, r.URL.String())
	log.Printf("proxying: method=%s target=%s", r.Method, r.URL.String())

	outReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "failed to build proxied request", http.StatusBadGateway)
		return
	}
	outReq.Header = r.Header.Clone()

	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "failed to reach proxy target: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleConnect implements CONNECT tunneling for an HTTPS target: hijack the
// client connection, dial the real target, confirm with "200 Connection
// Established" (the standard CONNECT response), then relay bytes in both
// directions until either side closes. Kept for completeness/parity with a
// real corporate proxy, even though this test suite's token endpoint
// (mock-oauth2-idp) is HTTP-only and never exercises this path today.
func handleConnect(w http.ResponseWriter, r *http.Request) {
	recordProxied("CONNECT", r.Host)
	log.Printf("proxying: CONNECT target=%s", r.Host)

	targetConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, "failed to reach CONNECT target: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT tunneling not supported by this server", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "failed to hijack connection", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetConn, clientConn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, targetConn)
	}()
	wg.Wait()
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"proxiedRequestCount": len(history),
		"history":             history,
	})
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	history = nil
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
