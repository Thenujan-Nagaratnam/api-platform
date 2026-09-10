// Command mock-introspecting-backend is a stand-in backend that, unlike
// mock-ai-backend, actually validates the Bearer token it receives - by
// calling a REAL OAuth2 introspection endpoint (RFC 7662, e.g. an Asgardeo
// tenant's /oauth2/introspect) with the same client_credentials used to
// obtain it. Used by the oauth2-realworld Postman collection's purge
// scenario (RW.5): postman-echo.com never rejects a revoked token, so it
// can't prove oauth2-generator's OnResponseHeaders purge logic actually
// fires against a real IdP. This backend gives a genuine 401 once the
// token is genuinely revoked at the real IdP, without needing any
// protected resource/admin access on the tenant beyond the M2M app's own
// client_credentials.
//
// Every request: extract the Authorization header, introspect the token
// against INTROSPECT_ENDPOINT using CLIENT_ID/CLIENT_SECRET (HTTP Basic,
// per RFC 7662). active:true -> 200, echoing the header back in the same
// shape postman-echo.com/headers uses ({"headers": {"authorization": ...}})
// so the collection's existing capture scripts work unchanged. Anything
// else (inactive, or the introspection call itself fails) -> 401.
//
// Configured entirely via env vars - ADDR, INTROSPECT_ENDPOINT, CLIENT_ID,
// CLIENT_SECRET - never hardcoded, and CLIENT_SECRET is never logged (see
// maskToken).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var introspectClient = &http.Client{Timeout: 8 * time.Second}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// maskToken keeps only enough of a bearer token to correlate log lines
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

func main() {
	addr := envOr("ADDR", ":9612")
	introspectEndpoint := os.Getenv("INTROSPECT_ENDPOINT")
	clientID := os.Getenv("CLIENT_ID")
	clientSecret := os.Getenv("CLIENT_SECRET")
	if introspectEndpoint == "" || clientID == "" || clientSecret == "" {
		log.Fatal("INTROSPECT_ENDPOINT, CLIENT_ID, and CLIENT_SECRET must all be set")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", handleAny(introspectEndpoint, clientID, clientSecret))

	log.Printf("mock-introspecting-backend listening on %s, introspecting against %s", addr, introspectEndpoint)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func handleAny(introspectEndpoint, clientID, clientSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		log.Printf("request: method=%s path=%s authorization=%s", r.Method, r.URL.Path, maskToken(authHeader))

		if token == "" || token == authHeader {
			respondUnauthorized(w, "missing or malformed Authorization header")
			return
		}

		active, err := introspectIsActive(introspectEndpoint, clientID, clientSecret, token)
		if err != nil {
			log.Printf("introspection call failed: %v", err)
			respondUnauthorized(w, "introspection call failed")
			return
		}
		if !active {
			respondUnauthorized(w, "token is not active (expired, revoked, or unknown)")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"headers": map[string]string{"authorization": authHeader},
		})
	}
}

func respondUnauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "token_rejected", "reason": reason})
}

// introspectIsActive calls the real RFC 7662 introspection endpoint and
// returns the "active" field of its response.
func introspectIsActive(endpoint, clientID, clientSecret, token string) (bool, error) {
	form := url.Values{"token": {token}}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return false, fmt.Errorf("build introspect request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := introspectClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("introspect request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("introspect endpoint returned status %d", resp.StatusCode)
	}

	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("decode introspect response: %w", err)
	}
	return body.Active, nil
}
