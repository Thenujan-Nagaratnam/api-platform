package semanticcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MistralEmbeddingProvider implements EmbeddingProvider for Mistral
type MistralEmbeddingProvider struct {
	authHeaderName string
	apiKey         string
	endpointURL    string
	model          string
	client         *http.Client
}

func NewMistralEmbeddingProvider(authHeaderName, apiKey, endpointURL, model string, timeout int) *MistralEmbeddingProvider {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &MistralEmbeddingProvider{
		authHeaderName: authHeaderName,
		apiKey:         apiKey,
		endpointURL:    endpointURL,
		model:          model,
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
	}
}

func (m *MistralEmbeddingProvider) GetType() string {
	return "MISTRAL"
}

func (m *MistralEmbeddingProvider) GetEmbedding(text string) ([]float32, error) {
	requestBody := map[string]string{
		"model": m.model,
		"input": text,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", m.endpointURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(m.authHeaderName, "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
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

	dataArray, ok := response["data"].([]interface{})
	if !ok || len(dataArray) == 0 {
		return nil, fmt.Errorf("no data found in embedding response")
	}
	firstItem, ok := dataArray[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid data format")
	}
	rawEmbedding, ok := firstItem["embedding"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("embedding field missing or invalid")
	}
	embedding := make([]float32, len(rawEmbedding))
	for i, val := range rawEmbedding {
		switch v := val.(type) {
		case float64:
			embedding[i] = float32(v)
		default:
			return nil, fmt.Errorf("unexpected value type in embedding: %T", v)
		}
	}
	return embedding, nil
}
