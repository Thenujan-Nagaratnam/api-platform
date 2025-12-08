package semanticcache

import (
	"fmt"

	policy "github.com/policy-engine/sdk/policy/v1alpha"
)

// SemanticCachePolicy implements semantic caching for LLM responses
// NOTE: This is a simplified implementation. Full implementation requires:
// - Embedding provider SDKs (OpenAI, Azure OpenAI, Mistral)
// - Vector database clients (Redis, Milvus)
// - Vector similarity search libraries
type SemanticCachePolicy struct{}

// NewPolicy creates a new SemanticCachePolicy instance
func NewPolicy() policy.Policy {
	return &SemanticCachePolicy{}
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

// Validate validates the policy configuration
func (p *SemanticCachePolicy) Validate(params map[string]interface{}) error {
	// Validate required parameters
	requiredParams := []string{
		"embeddingProvider", "embeddingEndpoint", "embeddingModel",
		"vectorStoreProvider", "threshold", "embeddingDimention",
		"dbHost", "dbPort",
	}

	for _, param := range requiredParams {
		if _, ok := params[param]; !ok {
			return fmt.Errorf("'%s' parameter is required", param)
		}
	}

	// Validate embedding provider
	if providerRaw, ok := params["embeddingProvider"]; ok {
		provider, ok := providerRaw.(string)
		if !ok {
			return fmt.Errorf("'embeddingProvider' must be a string")
		}
		validProviders := map[string]bool{"OPENAI": true, "AZURE_OPENAI": true, "MISTRAL": true}
		if !validProviders[provider] {
			return fmt.Errorf("'embeddingProvider' must be one of: OPENAI, AZURE_OPENAI, MISTRAL")
		}
	}

	// Validate vector store provider
	if providerRaw, ok := params["vectorStoreProvider"]; ok {
		provider, ok := providerRaw.(string)
		if !ok {
			return fmt.Errorf("'vectorStoreProvider' must be a string")
		}
		validProviders := map[string]bool{"REDIS": true, "MILVUS": true}
		if !validProviders[provider] {
			return fmt.Errorf("'vectorStoreProvider' must be one of: REDIS, MILVUS")
		}
	}

	// Validate threshold
	if thresholdRaw, ok := params["threshold"]; ok {
		threshold, ok := thresholdRaw.(float64)
		if !ok {
			return fmt.Errorf("'threshold' must be a number")
		}
		if threshold < 0.0 || threshold > 1.0 {
			return fmt.Errorf("'threshold' must be between 0.0 and 1.0")
		}
	}

	return nil
}

// OnRequest checks cache for semantically similar requests
func (p *SemanticCachePolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	// Check if request configuration exists
	requestParams, ok := params["request"]
	if !ok {
		// No request configuration, pass through
		return policy.UpstreamRequestModifications{}
	}

	// Extract request params (could be a map or the params themselves if no request/response separation)
	requestConfig, ok := requestParams.(map[string]interface{})
	if !ok {
		// If request is not a map, use params directly (backward compatibility)
		requestConfig = params
	}

	// Validate request config (placeholder - would use embeddingProvider, threshold, etc. from requestConfig)
	_ = requestConfig // Use requestConfig to avoid unused variable error

	if ctx.Body == nil || !ctx.Body.Present || len(ctx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	// TODO: Full implementation requires:
	// 1. Generate embedding for request body using embedding provider from requestConfig
	// 2. Search vector store for similar embeddings (above threshold from requestConfig)
	// 3. If cache hit, return cached response immediately
	// 4. If cache miss, store embedding in metadata for response phase

	// Placeholder: Store request body in metadata for response phase
	ctx.SharedContext.Metadata["semanticcache.request.body"] = string(ctx.Body.Content)
	ctx.SharedContext.Metadata["semanticcache.cache.hit"] = false

	// Continue to upstream (cache miss)
	return policy.UpstreamRequestModifications{}
}

// OnResponse stores response in cache if cache miss occurred
func (p *SemanticCachePolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	// Check if response configuration exists
	responseParams, ok := params["response"]
	if !ok {
		// No response configuration, pass through
		return policy.UpstreamResponseModifications{}
	}

	// Extract response params (could be a map or the params themselves if no request/response separation)
	responseConfig, ok := responseParams.(map[string]interface{})
	if !ok {
		// If response is not a map, use params directly (backward compatibility)
		responseConfig = params
	}

	// Validate response config (placeholder - would use vectorStoreProvider, dbHost, etc. from responseConfig)
	_ = responseConfig // Use responseConfig to avoid unused variable error

	if ctx.ResponseBody == nil || !ctx.ResponseBody.Present || len(ctx.ResponseBody.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	// Check if this was a cache miss
	if cacheHitRaw, ok := ctx.SharedContext.Metadata["semanticcache.cache.hit"]; ok {
		if cacheHit, ok := cacheHitRaw.(bool); ok && cacheHit {
			// Cache hit, nothing to store
			return policy.UpstreamResponseModifications{}
		}
	}

	// TODO: Full implementation requires:
	// 1. Retrieve embedding from metadata (stored in request phase)
	// 2. Store embedding + response body in vector store using responseConfig settings
	// 3. Associate with API ID for scoping

	// Placeholder: Log that response should be cached
	ctx.SharedContext.Metadata["semanticcache.response.stored"] = true

	return policy.UpstreamResponseModifications{}
}
