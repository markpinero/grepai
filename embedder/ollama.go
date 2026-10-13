package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	defaultOllamaEndpoint = "http://localhost:11434"
	defaultOllamaModel    = "nomic-embed-text"
	nomicEmbedDimensions  = 768

	// ollamaMaxBatchSize caps inputs per /api/embed request. Measured on
	// Ollama 0.35 with nomic-embed-text, throughput is flat or slightly worse
	// as inputs per request grow (about 22 chunks/s at 1 input, 16-19 at 8 to
	// 128), because the runner still embeds each sequence separately. Small
	// requests keep latency low and let the indexer overlap store round trips
	// while the model works. The batched path is about that overlap, not
	// about fewer HTTP calls.
	ollamaMaxBatchSize = 4
	// ollamaMaxBatchTokens bounds the estimated tokens per request so a run
	// of unusually large chunks does not produce a multi-second request.
	ollamaMaxBatchTokens = 4 * 2048
)

type OllamaEmbedder struct {
	endpoint    string
	model       string
	dimensions  int
	parallelism int
	client      *http.Client
}

// ollamaEmbedRequest targets /api/embed, which accepts a batch of inputs.
// Truncate is sent explicitly: the server default is true, which would
// silently embed a prefix of an oversized chunk instead of reporting a
// context-length error.
type ollamaEmbedRequest struct {
	Model    string   `json:"model"`
	Input    []string `json:"input"`
	Truncate bool     `json:"truncate"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type OllamaOption func(*OllamaEmbedder)

func WithOllamaEndpoint(endpoint string) OllamaOption {
	return func(e *OllamaEmbedder) {
		e.endpoint = endpoint
	}
}

func WithOllamaModel(model string) OllamaOption {
	return func(e *OllamaEmbedder) {
		e.model = model
	}
}
func WithOllamaDimensions(dimensions int) OllamaOption {
	return func(e *OllamaEmbedder) {
		e.dimensions = dimensions
	}
}

// WithOllamaTimeout overrides the HTTP client timeout for embedding requests.
// Values <= 0 are ignored, preserving the default.
func WithOllamaTimeout(d time.Duration) OllamaOption {
	return func(e *OllamaEmbedder) {
		if d > 0 && e.client != nil {
			e.client.Timeout = d
		}
	}
}

// WithOllamaParallelism sets the number of concurrent /api/embed requests
// used by EmbedBatches. Values <= 0 are ignored, preserving the default.
// Ollama serializes embedding requests on the runner (raising
// OLLAMA_NUM_PARALLEL did not change throughput in testing), so extra
// workers only keep the request queue full while the indexer does other work.
func WithOllamaParallelism(parallelism int) OllamaOption {
	return func(e *OllamaEmbedder) {
		if parallelism > 0 {
			e.parallelism = parallelism
		}
	}
}

func NewOllamaEmbedder(opts ...OllamaOption) *OllamaEmbedder {
	e := &OllamaEmbedder{
		endpoint:    defaultOllamaEndpoint,
		model:       defaultOllamaModel,
		dimensions:  nomicEmbedDimensions,
		parallelism: defaultParallelism,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	embeddings, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return embeddings[0], nil
}

// EmbedBatch embeds texts in a single request. If the request fails because
// an input exceeds the model context, it re-embeds the inputs one at a time
// to identify the offending index so the indexer can re-chunk it.
func (e *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	embeddings, err := e.embedRequest(ctx, texts, false)
	if err == nil {
		return embeddings, nil
	}
	if AsContextLengthError(err) == nil || len(texts) == 1 {
		if ctxErr := AsContextLengthError(err); ctxErr != nil {
			ctxErr.ChunkIndex = 0
			ctxErr.EstimatedTokens = EstimateTokens(texts[0])
		}
		return nil, err
	}

	results := make([][]float32, len(texts))
	for i, text := range texts {
		single, err := e.embedRequest(ctx, []string{text}, false)
		if err != nil {
			if ctxErr := AsContextLengthError(err); ctxErr != nil {
				ctxErr.ChunkIndex = i
				ctxErr.EstimatedTokens = EstimateTokens(text)
				return nil, ctxErr
			}
			return nil, fmt.Errorf("failed to embed text %d: %w", i, err)
		}
		results[i] = single[0]
	}
	return results, nil
}

// BatchLimits implements BatchSizer with request sizes suited to Ollama.
func (e *OllamaEmbedder) BatchLimits() (maxSize, maxTokens int) {
	return ollamaMaxBatchSize, ollamaMaxBatchTokens
}

// EmbedBatches implements BatchEmbedder. Batches are embedded concurrently by
// a bounded worker pool. A batch that fails on context length is retried one
// input at a time; an input that still does not fit is embedded with
// truncation so one oversized chunk does not abort the whole index run.
func (e *OllamaEmbedder) EmbedBatches(ctx context.Context, batches []Batch, progress BatchProgress) ([]BatchResult, error) {
	if len(batches) == 0 {
		return nil, nil
	}

	totalChunks := 0
	for _, batch := range batches {
		totalChunks += batch.Size()
	}

	var completedChunks atomic.Int64
	results := make([]BatchResult, len(batches))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(e.parallelism)

	for i := range batches {
		batch := batches[i]
		g.Go(func() error {
			embeddings, err := e.embedBatchLenient(gctx, batch.Contents())
			if err != nil {
				return fmt.Errorf("batch %d failed: %w", batch.Index, err)
			}
			results[batch.Index] = BatchResult{
				BatchIndex: batch.Index,
				Embeddings: embeddings,
			}
			if progress != nil {
				done := completedChunks.Add(int64(batch.Size()))
				progress(batch.Index, len(batches), int(done), totalChunks, false, 0, 0)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

func (e *OllamaEmbedder) embedBatchLenient(ctx context.Context, texts []string) ([][]float32, error) {
	embeddings, err := e.embedRequest(ctx, texts, false)
	if err == nil {
		return embeddings, nil
	}
	if AsContextLengthError(err) == nil {
		return nil, err
	}

	results := make([][]float32, len(texts))
	for i, text := range texts {
		single, err := e.embedRequest(ctx, []string{text}, false)
		if err != nil && AsContextLengthError(err) != nil {
			log.Printf("Warning: input of ~%d tokens exceeds the Ollama context; embedding truncated prefix", EstimateTokens(text))
			single, err = e.embedRequest(ctx, []string{text}, true)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to embed text %d: %w", i, err)
		}
		results[i] = single[0]
	}
	return results, nil
}

func (e *OllamaEmbedder) embedRequest(ctx context.Context, texts []string, truncate bool) ([][]float32, error) {
	reqBody := ollamaEmbedRequest{
		Model:    e.model,
		Input:    texts,
		Truncate: truncate,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/embed", e.endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)

		if isOllamaContextLengthError(bodyStr) {
			return nil, NewContextLengthError(0, 0, 0, bodyStr)
		}

		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, bodyStr)
	}

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(result.Embeddings))
	}
	for i, emb := range result.Embeddings {
		if len(emb) == 0 {
			return nil, fmt.Errorf("ollama returned empty embedding for input %d", i)
		}
	}

	return result.Embeddings, nil
}

// isOllamaContextLengthError matches the messages Ollama returns when an
// input does not fit the model context and truncation is disabled.
func isOllamaContextLengthError(body string) bool {
	return strings.Contains(body, "exceeds the context length") ||
		strings.Contains(body, "exceeds maximum context length") ||
		strings.Contains(body, "input length exceeds")
}

func (e *OllamaEmbedder) Dimensions() int {
	return e.dimensions
}

func (e *OllamaEmbedder) Close() error {
	return nil
}

// Ping checks if Ollama is reachable
func (e *OllamaEmbedder) Ping(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/tags", e.endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to reach Ollama at %s: %w", e.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	return nil
}
