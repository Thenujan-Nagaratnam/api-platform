// Command mock-azure-imds is a minimal stand-in for Azure's real Instance
// Metadata Service (IMDS) token endpoint, used to manually test the
// azure-managed-identity gateway policy end to end. The real IMDS
// (http://169.254.169.254/metadata/identity/oauth2/token) is only reachable
// from inside Azure compute, so this mock lets the policy be exercised
// anywhere by pointing systemParameters.imdsEndpoint at it instead.
//
// It is intentionally NOT a spec-complete IMDS - it implements exactly the
// request/response shape the policy needs, plus a small debug API so test
// flows can assert on gateway behavior (caching, refresh, failure handling)
// from the outside, mirroring mock-oauth2-idp's own conventions.
//
// Configured identity:
//   - valid client_id:   <CLIENT_ID> (default "11111111-1111-1111-1111-111111111111") -> 200 OK, fresh/cached token
//   - "broken-client"                                                                  -> 500 Internal Server Error (simulates IMDS/platform outage)
//   - "malformed-client"                                                               -> 200 OK, body missing access_token
//   - any other client_id                                                              -> 400, identity not found
//
// The Metadata: true header is required on every request, exactly like real
// IMDS - its absence is meant to prevent a browser or naive SSRF-via-XHR
// from ever reaching this endpoint.
//
// Endpoints:
//
//	GET  /metadata/identity/oauth2/token   api-version, resource (required), client_id
//	                                        (optional; validated against CLIENT_ID if
//	                                        given), optional `ttl` query param overrides
//	                                        expires_in/expires_on (seconds, default 300).
//	GET  /debug/stats    JSON summary of every token request received so far.
//	POST /debug/reset    Clears the request history.
//	GET  /healthz        Liveness probe.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const defaultTTLSeconds = 300

var (
	validClientID = envOr("CLIENT_ID", "11111111-1111-1111-1111-111111111111")

	mu       sync.Mutex
	tokenSeq int
	history  []tokenRequestRecord
)

type tokenRequestRecord struct {
	Time     time.Time `json:"time"`
	ClientID string    `json:"clientId"`
	Resource string    `json:"resource"`
	Outcome  string    `json:"outcome"` // "issued", "identity_not_found", "malformed", "server_error", "missing_metadata_header"
	Token    string    `json:"token,omitempty"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	addr := envOr("ADDR", ":9701")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metadata/identity/oauth2/token", handleToken)
	mux.HandleFunc("GET /debug/stats", handleStats)
	mux.HandleFunc("POST /debug/reset", handleReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("mock-azure-imds listening on %s (valid client_id: %s)", addr, validClientID)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func handleToken(w http.ResponseWriter, r *http.Request) {
	// Real IMDS requires this header on every request - a plain browser GET
	// or a naive server-side-request-forgery attempt can't set custom
	// headers on a simple redirected request, so this is IMDS's own baseline
	// defense. Enforcing it here keeps the mock's failure modes realistic.
	if r.Header.Get("Metadata") != "true" {
		recordRequest("", r.URL.Query().Get("resource"), "missing_metadata_header", "")
		writeJSONError(w, http.StatusBadRequest, "missing_metadata_header", "the Metadata: true header is required")
		return
	}

	resource := r.URL.Query().Get("resource")
	if resource == "" {
		recordRequest("", resource, "missing_metadata_header", "")
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "resource query parameter is required")
		return
	}
	clientID := r.URL.Query().Get("client_id")

	switch clientID {
	case "broken-client":
		recordRequest(clientID, resource, "server_error", "")
		http.Error(w, "internal server error (simulated IMDS/platform outage)", http.StatusInternalServerError)
		return

	case "malformed-client":
		// 200 OK but the body is missing access_token - exercises the
		// policy's "successful fetch, malformed response" failure path.
		recordRequest(clientID, resource, "malformed", "")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token_type":"Bearer","resource":"` + resource + `"}`))
		return

	case "", validClientID:
		// fall through to issue a token - an empty client_id is valid real
		// IMDS behavior when exactly one identity is attached (this mock
		// always has exactly one, so it behaves the same way).

	default:
		recordRequest(clientID, resource, "identity_not_found", "")
		writeJSONError(w, http.StatusBadRequest, "identity_not_found", "no user-assigned identity found matching the given client_id")
		return
	}

	ttl := defaultTTLSeconds
	if v := r.URL.Query().Get("ttl"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			ttl = parsed
		}
	}

	mu.Lock()
	tokenSeq++
	seq := tokenSeq
	mu.Unlock()

	// The token value embeds a sequence number and issue time so a test can
	// tell, just by comparing the string the gateway forwards upstream,
	// whether a cached token was reused or a fresh one was minted.
	token := fmt.Sprintf("mock-imds-token-%d-issued-%d", seq, time.Now().UnixNano())
	recordRequest(clientID, resource, "issued", token)

	now := time.Now()
	resp := map[string]interface{}{
		"access_token":   token,
		"client_id":      clientID,
		"expires_in":     strconv.Itoa(ttl),
		"expires_on":     strconv.FormatInt(now.Add(time.Duration(ttl)*time.Second).Unix(), 10),
		"ext_expires_in": strconv.Itoa(ttl),
		"not_before":     strconv.FormatInt(now.Unix(), 10),
		"resource":       resource,
		"token_type":     "Bearer",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func recordRequest(clientID, resource, outcome, token string) {
	mu.Lock()
	defer mu.Unlock()
	history = append(history, tokenRequestRecord{
		Time:     time.Now().UTC(),
		ClientID: clientID,
		Resource: resource,
		Outcome:  outcome,
		Token:    token,
	})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"tokenRequestCount": len(history),
		"history":           history,
	})
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	history = nil
	tokenSeq = 0
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func writeJSONError(w http.ResponseWriter, status int, errCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             errCode,
		"error_description": description,
	})
}
