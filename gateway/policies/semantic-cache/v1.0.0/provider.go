package semanticcache

import "context"

// EmbeddingProvider defines the interface for embedding providers
type EmbeddingProvider interface {
	GetEmbedding(text string) ([]float32, error)
	GetType() string
}

// VectorDBProvider defines the interface for vector database providers
type VectorDBProvider interface {
	Store(embedding []float32, responseData map[string]interface{}, apiID string, ctx context.Context) error
	Retrieve(embedding []float32, threshold float64, apiID string, ctx context.Context) (map[string]interface{}, error)
	GetType() string
	Close() error
}
