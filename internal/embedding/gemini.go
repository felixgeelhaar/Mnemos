package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// GeminiEmbedder calls the Google Gemini embedding API.
type GeminiEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewGeminiEmbedder creates an embedding client for the Google Gemini API.
func NewGeminiEmbedder(baseURL, apiKey, model string) *GeminiEmbedder {
	return &GeminiEmbedder{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		http:    defaultEmbeddingHTTPClient(),
	}
}

type geminiBatchEmbedRequest struct {
	Requests []geminiEmbedRequest `json:"requests"`
}

type geminiEmbedRequest struct {
	Model   string             `json:"model"`
	Content geminiEmbedContent `json:"content"`
}

type geminiEmbedContent struct {
	Parts []geminiEmbedPart `json:"parts"`
}

type geminiEmbedPart struct {
	Text string `json:"text"`
}

type geminiBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// ModelID satisfies [ModelIdentifier], reporting the configured model so
// stored vectors are stamped and recall filters to this model space.
func (c *GeminiEmbedder) ModelID() string { return c.model }

// Embed generates embeddings for the given texts using the Gemini batchEmbedContents API.
func (c *GeminiEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	requests := make([]geminiEmbedRequest, 0, len(texts))
	for _, text := range texts {
		requests = append(requests, geminiEmbedRequest{
			Model: "models/" + c.model,
			Content: geminiEmbedContent{
				Parts: []geminiEmbedPart{{Text: text}},
			},
		})
	}

	reqBody := geminiBatchEmbedRequest{Requests: requests}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini embedding request: %w", err)
	}

	// The key goes in a header, never the query string — a ?key= URL is
	// echoed back inside *url.Error on any transport failure, and those
	// errors are logged and surfaced to MCP clients. See the matching
	// comment in internal/llm/gemini.go.
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:batchEmbedContents", c.baseURL, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create gemini embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini embedding request failed: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("close body: %v", err)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read gemini embedding response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini embedding returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result geminiBatchEmbedResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal gemini embedding response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("gemini embedding error %d (%s): %s", result.Error.Code, result.Error.Status, result.Error.Message)
	}

	vectors := make([][]float32, 0, len(result.Embeddings))
	for _, e := range result.Embeddings {
		vectors = append(vectors, e.Values)
	}
	if err := validateVectors(vectors, len(texts), "gemini"); err != nil {
		return nil, err
	}
	return vectors, nil
}
