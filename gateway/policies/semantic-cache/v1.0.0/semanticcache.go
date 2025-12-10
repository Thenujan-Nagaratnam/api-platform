package semanticcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	policy "github.com/wso2/api-platform/sdk/gateway/policy/v1alpha"
)

// SemanticCachePolicy implements semantic caching for LLM responses
type SemanticCachePolicy struct {
	mu sync.RWMutex
	// Cache providers per policy instance
	embeddingProviders map[string]EmbeddingProvider
	vectorDBProviders  map[string]VectorDBProvider
}

// NewPolicy creates a new SemanticCachePolicy instance
func NewPolicy() policy.Policy {
	return &SemanticCachePolicy{
		embeddingProviders: make(map[string]EmbeddingProvider),
		vectorDBProviders:  make(map[string]VectorDBProvider),
	}
}

// Mode returns the processing mode for this policy
func (p *SemanticCachePolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration (empty as requested)
func (p *SemanticCachePolicy) Validate(params map[string]interface{}) error {
	return nil
}

// validateParams validates all required parameters
func (p *SemanticCachePolicy) validateParams(params map[string]interface{}) error {
	// Validate embedding provider
	embeddingProviderRaw, ok := params["embeddingProvider"]
	if !ok {
		return fmt.Errorf("'embeddingProvider' parameter is required")
	}
	embeddingProvider, ok := embeddingProviderRaw.(string)
	if !ok {
		return fmt.Errorf("'embeddingProvider' must be a string")
	}
	if embeddingProvider != "OPENAI" && embeddingProvider != "MISTRAL" && embeddingProvider != "AZURE_OPENAI" {
		return fmt.Errorf("'embeddingProvider' must be one of: OPENAI, MISTRAL, AZURE_OPENAI")
	}

	// Validate embedding endpoint
	embeddingEndpointRaw, ok := params["embeddingEndpoint"]
	if !ok {
		return fmt.Errorf("'embeddingEndpoint' parameter is required")
	}
	_, ok = embeddingEndpointRaw.(string)
	if !ok {
		return fmt.Errorf("'embeddingEndpoint' must be a string")
	}

	// Validate embedding model
	embeddingModelRaw, ok := params["embeddingModel"]
	if !ok {
		return fmt.Errorf("'embeddingModel' parameter is required")
	}
	_, ok = embeddingModelRaw.(string)
	if !ok {
		return fmt.Errorf("'embeddingModel' must be a string")
	}

	// Validate API key
	apiKeyRaw, ok := params["apiKey"]
	if !ok {
		return fmt.Errorf("'apiKey' parameter is required")
	}
	_, ok = apiKeyRaw.(string)
	if !ok {
		return fmt.Errorf("'apiKey' must be a string")
	}

	// Validate vector store provider
	vectorStoreProviderRaw, ok := params["vectorStoreProvider"]
	if !ok {
		return fmt.Errorf("'vectorStoreProvider' parameter is required")
	}
	vectorStoreProvider, ok := vectorStoreProviderRaw.(string)
	if !ok {
		return fmt.Errorf("'vectorStoreProvider' must be a string")
	}
	if vectorStoreProvider != "REDIS" && vectorStoreProvider != "MILVUS" {
		return fmt.Errorf("'vectorStoreProvider' must be one of: REDIS, MILVUS")
	}

	// Validate threshold
	thresholdRaw, ok := params["threshold"]
	if !ok {
		return fmt.Errorf("'threshold' parameter is required")
	}
	threshold, ok := thresholdRaw.(float64)
	if !ok {
		if thresholdStr, ok := thresholdRaw.(string); ok {
			var err error
			threshold, err = strconv.ParseFloat(thresholdStr, 64)
			if err != nil {
				return fmt.Errorf("'threshold' must be a number")
			}
		} else {
			return fmt.Errorf("'threshold' must be a number")
		}
	}
	if threshold < 0.0 || threshold > 1.0 {
		return fmt.Errorf("'threshold' must be between 0.0 and 1.0")
	}

	// Validate optional parameters
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		_, ok := jsonPathRaw.(string)
		if !ok {
			return fmt.Errorf("'jsonPath' must be a string")
		}
	}

	if embeddingDimensionRaw, ok := params["embeddingDimension"]; ok {
		_, ok := embeddingDimensionRaw.(float64)
		if !ok {
			if dimStr, ok := embeddingDimensionRaw.(string); ok {
				_, err := strconv.Atoi(dimStr)
				if err != nil {
					return fmt.Errorf("'embeddingDimension' must be an integer")
				}
			} else {
				return fmt.Errorf("'embeddingDimension' must be an integer")
			}
		}
	}

	return nil
}

// getOrCreateEmbeddingProvider gets or creates an embedding provider
func (p *SemanticCachePolicy) getOrCreateEmbeddingProvider(params map[string]interface{}) (EmbeddingProvider, error) {
	embeddingProvider, _ := params["embeddingProvider"].(string)
	embeddingEndpoint, _ := params["embeddingEndpoint"].(string)
	embeddingModel, _ := params["embeddingModel"].(string)
	apiKey, _ := params["apiKey"].(string)
	headerName, _ := params["headerName"].(string)
	if headerName == "" {
		headerName = "Authorization"
	}

	// Create a unique key for this provider configuration
	providerKey := fmt.Sprintf("%s:%s:%s", embeddingProvider, embeddingEndpoint, embeddingModel)

	p.mu.RLock()
	if provider, exists := p.embeddingProviders[providerKey]; exists {
		p.mu.RUnlock()
		return provider, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check again after acquiring lock
	if provider, exists := p.embeddingProviders[providerKey]; exists {
		return provider, nil
	}

	var provider EmbeddingProvider
	timeout := DefaultTimeout

	switch embeddingProvider {
	case "OPENAI":
		provider = NewOpenAIEmbeddingProvider(headerName, apiKey, embeddingEndpoint, embeddingModel, timeout)
	case "MISTRAL":
		provider = NewMistralEmbeddingProvider(headerName, apiKey, embeddingEndpoint, embeddingModel, timeout)
	case "AZURE_OPENAI":
		provider = NewAzureOpenAIEmbeddingProvider(headerName, apiKey, embeddingEndpoint, timeout)
	default:
		return nil, fmt.Errorf("unsupported embedding provider: %s", embeddingProvider)
	}

	p.embeddingProviders[providerKey] = provider
	return provider, nil
}

// getOrCreateVectorDBProvider gets or creates a vector DB provider
func (p *SemanticCachePolicy) getOrCreateVectorDBProvider(params map[string]interface{}) (VectorDBProvider, error) {
	vectorStoreProvider, _ := params["vectorStoreProvider"].(string)
	dbHost, _ := params["dbHost"].(string)
	dbPortRaw, _ := params["dbPort"].(float64)
	dbPort := int(dbPortRaw)
	username, _ := params["username"].(string)
	password, _ := params["password"].(string)
	database, _ := params["database"].(string)

	embeddingDimensionRaw, _ := params["embeddingDimension"].(float64)
	embeddingDimension := int(embeddingDimensionRaw)
	if embeddingDimension == 0 {
		// Default dimensions based on provider
		embeddingDimension = 1536 // OpenAI ada-002 default
	}

	ttl := DefaultTTL

	// Create a unique key for this provider configuration
	providerKey := fmt.Sprintf("%s:%s:%d:%s", vectorStoreProvider, dbHost, dbPort, database)

	p.mu.RLock()
	if provider, exists := p.vectorDBProviders[providerKey]; exists {
		p.mu.RUnlock()
		return provider, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check again after acquiring lock
	if provider, exists := p.vectorDBProviders[providerKey]; exists {
		return provider, nil
	}

	var provider VectorDBProvider
	var err error

	switch vectorStoreProvider {
	case "REDIS":
		provider, err = NewRedisVectorDBProvider(dbHost, dbPort, username, password, database, embeddingDimension, ttl)
		if err != nil {
			return nil, fmt.Errorf("failed to create Redis provider: %w", err)
		}
	case "MILVUS":
		provider, err = NewMilvusVectorDBProvider(dbHost, dbPort, username, password, database, embeddingDimension, ttl)
		if err != nil {
			return nil, fmt.Errorf("failed to create Milvus provider: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported vector store provider: %s", vectorStoreProvider)
	}

	p.vectorDBProviders[providerKey] = provider
	return provider, nil
}

// OnRequest checks for cached response using semantic similarity
func (p *SemanticCachePolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	// Validate parameters
	if err := p.validateParams(params); err != nil {
		return policy.UpstreamRequestModifications{}
	}

	if ctx.Body.Content == nil || len(ctx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	// Extract text using jsonPath if provided
	jsonPath, _ := params["jsonPath"].(string)
	text, err := extractStringValueFromJSONPath(ctx.Body.Content, jsonPath)
	if err != nil {
		// If extraction fails, use full body
		text = string(ctx.Body.Content)
	}

	// Get embedding provider
	embeddingProvider, err := p.getOrCreateEmbeddingProvider(params)
	if err != nil {
		// If provider creation fails, pass through
		return policy.UpstreamRequestModifications{}
	}

	// Generate embedding
	embedding, err := embeddingProvider.GetEmbedding(text)
	if err != nil {
		// If embedding generation fails, pass through
		return policy.UpstreamRequestModifications{}
	}

	// Store embedding in metadata for response phase
	embeddingBytes, err := json.Marshal(embedding)
	if err == nil {
		ctx.Metadata[MetadataKeyEmbedding] = string(embeddingBytes)
	}

	// Get vector DB provider
	vectorDBProvider, err := p.getOrCreateVectorDBProvider(params)
	if err != nil {
		// If provider creation fails, pass through
		return policy.UpstreamRequestModifications{}
	}

	// Get threshold
	threshold, _ := params["threshold"].(float64)
	if threshold == 0 {
		threshold = 0.8 // Default threshold
	}

	// Get API ID from metadata or use default
	apiID, _ := ctx.Metadata["apiID"].(string)
	if apiID == "" {
		apiID = "default"
	}

	// Check cache
	cachedResponse, err := vectorDBProvider.Retrieve(embedding, threshold, apiID, context.Background())
	if err != nil {
		// Cache miss or error, continue to upstream
		return policy.UpstreamRequestModifications{}
	}

	// Cache hit - return cached response
	responseBodyBytes, err := json.Marshal(cachedResponse)
	if err != nil {
		return policy.UpstreamRequestModifications{}
	}

	return policy.ImmediateResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":     "application/json",
			"X-Cache-Status":   "HIT",
			"X-Cache-Provider": "semantic",
		},
		Body: responseBodyBytes,
	}
}

// OnResponse stores response in cache for future semantic lookups
func (p *SemanticCachePolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	// Only cache successful responses
	if ctx.ResponseStatus != 200 {
		return policy.UpstreamResponseModifications{}
	}

	// Check if embedding was generated in request phase
	embeddingRaw, exists := ctx.Metadata[MetadataKeyEmbedding]
	if !exists {
		return policy.UpstreamResponseModifications{}
	}

	embeddingStr, ok := embeddingRaw.(string)
	if !ok {
		return policy.UpstreamResponseModifications{}
	}

	var embedding []float32
	if err := json.Unmarshal([]byte(embeddingStr), &embedding); err != nil {
		return policy.UpstreamResponseModifications{}
	}

	if ctx.ResponseBody.Content == nil || len(ctx.ResponseBody.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	// Parse response body
	var responseData map[string]interface{}
	if err := json.Unmarshal(ctx.ResponseBody.Content, &responseData); err != nil {
		// If response is not JSON, skip caching
		return policy.UpstreamResponseModifications{}
	}

	// Get vector DB provider
	vectorDBProvider, err := p.getOrCreateVectorDBProvider(params)
	if err != nil {
		// If provider creation fails, continue
		return policy.UpstreamResponseModifications{}
	}

	// Get API ID from metadata or use default
	apiID, _ := ctx.Metadata["apiID"].(string)
	if apiID == "" {
		apiID = "default"
	}

	// Store in cache
	err = vectorDBProvider.Store(embedding, responseData, apiID, context.Background())
	if err != nil {
		// If cache storage fails, continue (don't block response)
		return policy.UpstreamResponseModifications{}
	}

	return policy.UpstreamResponseModifications{}
}
