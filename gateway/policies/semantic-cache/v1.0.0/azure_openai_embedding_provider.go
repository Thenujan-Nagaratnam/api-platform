package semanticcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AzureOpenAIEmbeddingProvider implements EmbeddingProvider for Azure OpenAI
type AzureOpenAIEmbeddingProvider struct {
	authHeaderName string
	apiKey         string
	endpointURL    string
	client         *http.Client
}

func NewAzureOpenAIEmbeddingProvider(authHeaderName, apiKey, endpointURL string, timeout int) *AzureOpenAIEmbeddingProvider {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &AzureOpenAIEmbeddingProvider{
		authHeaderName: authHeaderName,
		apiKey:         apiKey,
		endpointURL:    endpointURL,
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
	}
}

func (a *AzureOpenAIEmbeddingProvider) GetType() string {
	return "AZURE_OPENAI"
}

func (a *AzureOpenAIEmbeddingProvider) GetEmbedding(text string) ([]float32, error) {
	requestBody := map[string]interface{}{
		"input": text,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", a.endpointURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(a.authHeaderName, a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
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
