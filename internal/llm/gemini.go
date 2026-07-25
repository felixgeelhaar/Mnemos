package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GeminiClient calls the Google Gemini GenerateContent API.
type GeminiClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewGeminiClient creates a client for the Google Gemini API.
func NewGeminiClient(baseURL, apiKey, model string) *GeminiClient {
	return &GeminiClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		http:    defaultLLMHTTPClient(),
	}
}

type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens  int    `json:"maxOutputTokens"`
	ResponseMimeType string `json:"responseMimeType,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Complete sends a completion request to the Gemini API.
func (c *GeminiClient) Complete(ctx context.Context, messages []Message) (Response, error) {
	var system *geminiContent
	var contents []geminiContent

	for _, m := range messages {
		if m.Role == RoleSystem {
			system = &geminiContent{
				Parts: []geminiPart{{Text: m.Content}},
			}
			continue
		}
		role := "user"
		if m.Role == RoleAssistant {
			role = "model"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		})
	}

	genConfig := geminiGenerationConfig{MaxOutputTokens: 4096}

	// Enable JSON mode when the system prompt asks for JSON output.
	for _, m := range messages {
		if m.Role == RoleSystem && strings.Contains(m.Content, "JSON") {
			genConfig.ResponseMimeType = "application/json"
			break
		}
	}

	reqBody := geminiRequest{
		Contents:          contents,
		SystemInstruction: system,
		GenerationConfig:  genConfig,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return Response{}, fmt.Errorf("marshal gemini request: %w", err)
	}

	// The key goes in a header, never the query string. A ?key= URL ends up
	// inside *url.Error on any transport failure — Go redacts userinfo there
	// but never the query — and those errors are logged and returned to MCP
	// clients, which would put a live key in an agent transcript.
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", c.baseURL, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("create gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("gemini request failed: %w", err)
	}
	defer closeBody(resp.Body)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read gemini response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("gemini returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result geminiResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return Response{}, fmt.Errorf("unmarshal gemini response: %w", err)
	}

	if result.Error != nil {
		return Response{}, fmt.Errorf("gemini error %d (%s): %s", result.Error.Code, result.Error.Status, result.Error.Message)
	}

	if len(result.Candidates) == 0 {
		return Response{}, fmt.Errorf("gemini returned no candidates")
	}

	var text string
	for _, part := range result.Candidates[0].Content.Parts {
		text += part.Text
	}

	return Response{
		Content:      text,
		Model:        c.model,
		InputTokens:  result.UsageMetadata.PromptTokenCount,
		OutputTokens: result.UsageMetadata.CandidatesTokenCount,
	}, nil
}
