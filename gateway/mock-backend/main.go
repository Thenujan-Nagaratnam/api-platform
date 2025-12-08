package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type MockBackend struct {
	port int
}

type ResponseConfig struct {
	StatusCode      int               `json:"status_code,omitempty"`
	Body            interface{}       `json:"body,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Delay           int               `json:"delay_ms,omitempty"`
	WordCount       int               `json:"word_count,omitempty"`
	SentenceCount   int               `json:"sentence_count,omitempty"`
	ContentLength   int               `json:"content_length,omitempty"`
	IncludeURLs     []string          `json:"include_urls,omitempty"`
	IncludePII      bool              `json:"include_pii,omitempty"`
	IncludePassword bool              `json:"include_password,omitempty"`
}

func main() {
	port := 8081
	if p := os.Getenv("PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}

	backend := &MockBackend{port: port}
	backend.setupRoutes()

	log.Printf("Mock Backend Server starting on port %d", port)
	log.Printf("Health check: http://localhost:%d/health", port)
	log.Printf("Echo endpoint: http://localhost:%d/echo", port)
	log.Printf("Configurable endpoint: http://localhost:%d/mock", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

func (m *MockBackend) setupRoutes() {
	http.HandleFunc("/health", m.handleHealth)
	http.HandleFunc("/echo", m.handleEcho)
	http.HandleFunc("/mock", m.handleMock)
	http.HandleFunc("/", m.handleDefault)
}

func (m *MockBackend) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}

func (m *MockBackend) handleEcho(w http.ResponseWriter, r *http.Request) {
	// Echo back the request
	response := map[string]interface{}{
		"method":      r.Method,
		"path":        r.URL.Path,
		"query":       r.URL.Query(),
		"headers":     r.Header,
		"remote_addr": r.RemoteAddr,
		"timestamp":   time.Now().Format(time.RFC3339),
	}

	// Read and include request body if present
	if r.Body != nil {
		var bodyData interface{}
		if err := json.NewDecoder(r.Body).Decode(&bodyData); err == nil {
			response["body"] = bodyData
		} else {
			// If not JSON, read as string
			r.Body.Close()
			r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024) // 10MB max
			buf := make([]byte, 1024)
			if n, err := r.Body.Read(buf); err == nil {
				response["body_raw"] = string(buf[:n])
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func (m *MockBackend) handleMock(w http.ResponseWriter, r *http.Request) {
	var config ResponseConfig

	// Parse configuration from query parameters or request body
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			// Try query parameters instead
			config = m.parseConfigFromQuery(r)
		}
	} else {
		config = m.parseConfigFromQuery(r)
	}

	// Apply delay if specified
	if config.Delay > 0 {
		time.Sleep(time.Duration(config.Delay) * time.Millisecond)
	}

	// Set status code
	statusCode := config.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	// Set headers
	for key, value := range config.Headers {
		w.Header().Set(key, value)
	}

	// Generate response body
	body := m.generateResponseBody(r, config)

	// Set content type if not already set
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}

	w.WriteHeader(statusCode)

	if bodyStr, ok := body.(string); ok {
		w.Write([]byte(bodyStr))
	} else {
		json.NewEncoder(w).Encode(body)
	}
}

func (m *MockBackend) parseConfigFromQuery(r *http.Request) ResponseConfig {
	config := ResponseConfig{}

	if statusStr := r.URL.Query().Get("status"); statusStr != "" {
		if status, err := strconv.Atoi(statusStr); err == nil {
			config.StatusCode = status
		}
	}

	if delayStr := r.URL.Query().Get("delay"); delayStr != "" {
		if delay, err := strconv.Atoi(delayStr); err == nil {
			config.Delay = delay
		}
	}

	if wordCountStr := r.URL.Query().Get("word_count"); wordCountStr != "" {
		if wordCount, err := strconv.Atoi(wordCountStr); err == nil {
			config.WordCount = wordCount
		}
	}

	if sentenceCountStr := r.URL.Query().Get("sentence_count"); sentenceCountStr != "" {
		if sentenceCount, err := strconv.Atoi(sentenceCountStr); err == nil {
			config.SentenceCount = sentenceCount
		}
	}

	if contentLengthStr := r.URL.Query().Get("content_length"); contentLengthStr != "" {
		if contentLength, err := strconv.Atoi(contentLengthStr); err == nil {
			config.ContentLength = contentLength
		}
	}

	if includePII := r.URL.Query().Get("include_pii"); includePII == "true" {
		config.IncludePII = true
	}

	if includePassword := r.URL.Query().Get("include_password"); includePassword == "true" {
		config.IncludePassword = true
	}

	if urls := r.URL.Query().Get("include_urls"); urls != "" {
		config.IncludeURLs = strings.Split(urls, ",")
	}

	return config
}

func (m *MockBackend) generateResponseBody(r *http.Request, config ResponseConfig) interface{} {
	// If custom body is provided, use it
	if config.Body != nil {
		return config.Body
	}

	// Build response based on configuration
	response := map[string]interface{}{
		"method":    r.Method,
		"path":      r.URL.Path,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	// Include request body if present
	if r.Body != nil && r.Method != "GET" {
		var bodyData interface{}
		if err := json.NewDecoder(r.Body).Decode(&bodyData); err == nil {
			response["request_body"] = bodyData
		}
	}

	// Generate message with specific word count
	if config.WordCount > 0 {
		response["message"] = m.generateTextWithWordCount(config.WordCount)
	}

	// Generate message with specific sentence count
	if config.SentenceCount > 0 {
		response["message"] = m.generateTextWithSentenceCount(config.SentenceCount)
	}

	// Generate message with specific content length
	if config.ContentLength > 0 {
		response["message"] = m.generateTextWithContentLength(config.ContentLength)
	}

	// Include PII data if requested
	if config.IncludePII {
		response["user"] = map[string]interface{}{
			"name":        "John Doe",
			"email":       "john.doe@example.com",
			"phone":       "+1-555-123-4567",
			"ssn":         "123-45-6789",
			"address":     "123 Main St, Anytown, ST 12345",
			"credit_card": "4532-1234-5678-9010",
		}
	}

	// Include password if requested
	if config.IncludePassword {
		response["credentials"] = map[string]string{
			"username": "admin",
			"password": "secret123",
		}
	}

	// Include URLs if requested
	if len(config.IncludeURLs) > 0 {
		response["links"] = config.IncludeURLs
		response["message"] = fmt.Sprintf("Visit %s for more information", strings.Join(config.IncludeURLs, " and "))
	}

	// Default message if nothing specified
	if response["message"] == nil {
		response["message"] = "Mock backend response"
		response["status"] = "success"
	}

	return response
}

func (m *MockBackend) generateTextWithWordCount(count int) string {
	words := []string{"word", "test", "sample", "data", "content", "message", "response", "request", "api", "backend"}
	result := []string{}
	for i := 0; i < count; i++ {
		result = append(result, words[i%len(words)])
	}
	return strings.Join(result, " ")
}

func (m *MockBackend) generateTextWithSentenceCount(count int) string {
	sentences := []string{
		"This is the first sentence.",
		"This is the second sentence.",
		"This is the third sentence.",
		"This is the fourth sentence.",
		"This is the fifth sentence.",
	}
	result := []string{}
	for i := 0; i < count; i++ {
		result = append(result, sentences[i%len(sentences)])
	}
	return strings.Join(result, " ")
}

func (m *MockBackend) generateTextWithContentLength(length int) string {
	baseText := "This is a sample text for content length testing. "
	repeatCount := length / len(baseText)
	if repeatCount == 0 {
		repeatCount = 1
	}
	result := strings.Repeat(baseText, repeatCount)
	// Trim to exact length
	if len(result) > length {
		result = result[:length]
	}
	return result
}

func (m *MockBackend) handleDefault(w http.ResponseWriter, r *http.Request) {
	// Default handler - echo the request
	m.handleEcho(w, r)
}
