package semanticcache

import (
	"fmt"
	"os"
	"sync"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

// HugotEmbeddingProvider implements EmbeddingProvider for Hugot (local ONNX models)
type HugotEmbeddingProvider struct {
	session           *hugot.Session
	pipeline          *pipelines.FeatureExtractionPipeline
	modelPath         string
	modelName         string
	modelDownloadPath string
	mu                sync.Mutex
}

var (
	hugotSession *hugot.Session
	hugotMu      sync.Mutex
)

// NewHugotEmbeddingProvider creates a new Hugot embedding provider
// modelName: e.g., "sentence-transformers/all-MiniLM-L6-v2"
// modelDownloadPath: directory where models are stored/downloaded
func NewHugotEmbeddingProvider(modelName, modelDownloadPath string) (*HugotEmbeddingProvider, error) {
	// Use a shared session for all Hugot providers (more efficient)
	hugotMu.Lock()
	if hugotSession == nil {
		session, err := hugot.NewGoSession()
		if err != nil {
			hugotMu.Unlock()
			return nil, fmt.Errorf("failed to create Hugot session: %w", err)
		}
		hugotSession = session
	}
	session := hugotSession
	hugotMu.Unlock()

	// Download the model if needed
	fmt.Fprintf(os.Stderr, "[Hugot] Starting model download for %s to %s\n", modelName, modelDownloadPath)
	downloadOptions := hugot.NewDownloadOptions()
	downloadOptions.OnnxFilePath = "onnx/model.onnx"
	modelPath, err := hugot.DownloadModel(modelName, modelDownloadPath, downloadOptions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Hugot] Model download failed: %v\n", err)
		return nil, fmt.Errorf("failed to download model: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[Hugot] Model downloaded to %s\n", modelPath)

	// Create feature extraction pipeline configuration
	config := hugot.FeatureExtractionConfig{
		ModelPath: modelPath,
		Name:      "embeddingPipeline",
	}

	// Create the feature extraction pipeline (this can take time on first initialization)
	fmt.Fprintf(os.Stderr, "[Hugot] Creating pipeline (this may take 30-60 seconds on first run)...\n")
	pipeline, err := hugot.NewPipeline[*pipelines.FeatureExtractionPipeline](session, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Hugot] Pipeline creation failed: %v\n", err)
		return nil, fmt.Errorf("failed to create pipeline: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[Hugot] Pipeline created successfully\n")

	return &HugotEmbeddingProvider{
		session:           session,
		pipeline:          pipeline,
		modelPath:         modelPath,
		modelName:         modelName,
		modelDownloadPath: modelDownloadPath,
	}, nil
}

func (h *HugotEmbeddingProvider) GetType() string {
	return "HUGOT"
}

func (h *HugotEmbeddingProvider) GetEmbedding(text string) ([]float32, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Generate embedding using Hugot
	embeddingResult, err := h.pipeline.RunPipeline([]string{text})
	if err != nil {
		return nil, fmt.Errorf("failed to generate embedding: %w", err)
	}

	if len(embeddingResult.Embeddings) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}

	return embeddingResult.Embeddings[0], nil
}

// Close cleans up the Hugot session (shared, so we don't close it here)
func (h *HugotEmbeddingProvider) Close() error {
	// Session is shared, don't close it here
	return nil
}
