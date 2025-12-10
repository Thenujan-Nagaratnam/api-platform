package semanticcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIEmbeddingProvider implements EmbeddingProvider for OpenAI
type OpenAIEmbeddingProvider struct {
	authHeaderName string
	apiKey         string
	endpointURL    string
	model          string
	client         *http.Client
}

func NewOpenAIEmbeddingProvider(authHeaderName, apiKey, endpointURL, model string, timeout int) *OpenAIEmbeddingProvider {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &OpenAIEmbeddingProvider{
		authHeaderName: authHeaderName,
		apiKey:         apiKey,
		endpointURL:    endpointURL,
		model:          model,
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
	}
}

func (o *OpenAIEmbeddingProvider) GetType() string {
	return "OPENAI"
}

func (o *OpenAIEmbeddingProvider) GetEmbedding(text string) ([]float32, error) {
	requestBody := map[string]interface{}{
		"model": o.model,
		"input": text,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", o.endpointURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(o.authHeaderName, "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response map[string]interface{}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, err
	}

	data := response["data"].([]interface{})[0].(map[string]interface{})
	embedding := data["embedding"].([]interface{})
	embeddingResult := make([]float32, len(embedding))
	for i, value := range embedding {
		embeddingResult[i] = float32(value.(float64))
	}

	return embeddingResult, nil
}
