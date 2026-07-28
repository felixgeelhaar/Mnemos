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

// OpenAIClient calls any OpenAI-compatible Chat Completions API.
// Works with OpenAI, Azure OpenAI, Ollama, Groq, Together, Fireworks,
// Mistral, vLLM, LM Studio, and any other compatible endpoint.
type OpenAIClient struct {
	baseURL  string
	apiKey   string
	model    string
	provider string // for error labels: "openai", "ollama", "openai-compat"
	http     *http.Client
}

// NewOpenAIClient creates a client for any OpenAI-compatible API.
// The provider label is used in error messages so users see the provider
// they actually configured (e.g. "ollama response" not "openai response").
func NewOpenAIClient(baseURL, apiKey, model, provider string) *OpenAIClient {
	if provider == "" {
		provider = "openai"
	}
	return &OpenAIClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		model:    model,
		provider: provider,
		http:     defaultLLMHTTPClient(),
	}
}

type openAIRequest struct {
	Model          string             `json:"model"`
	Messages       []openAIMessage    `json:"messages"`
	Stream         bool               `json:"stream"` // explicit false: Ollama /api/chat defaults to streaming
	ResponseFormat *openAIResponseFmt `json:"response_format,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIResponseFmt enables JSON mode when the system prompt requests JSON.
type openAIResponseFmt struct {
	Type string `json:"type"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete sends a completion request to an OpenAI-compatible API.
func (c *OpenAIClient) Complete(ctx context.Context, messages []Message) (Response, error) {
	apiMsgs := make([]openAIMessage, 0, len(messages))
	for _, m := range messages {
		apiMsgs = append(apiMsgs, openAIMessage{
			Role:    string(m.Role),
			Content: m.Content,
		})
	}

	reqBody := openAIRequest{
		Model:    c.model,
		Messages: apiMsgs,
		Stream:   false,
	}

	// Enable JSON mode when the system prompt asks for a JSON OBJECT.
	//
	// The qualifier is the whole point. `response_format: json_object` constrains
	// the model to emit a top-level OBJECT, so a prompt asking for a top-level
	// ARRAY cannot be satisfied — and models resolve that contradiction by
	// returning `{}`. Valid JSON, constraint honoured, no content.
	//
	// Measured against a live qwen2.5:3b with mnemos's own extraction prompt:
	//
	//	without response_format : [{"text":"…","type":"decision","entities":[…]}]
	//	with    json_object     : {}
	//
	// `{}` then fails to unmarshal into []llmClaim, and extractBatch treats a
	// parse error as "the model misbehaved" and falls back to rule-based
	// extraction — silently. So LLM extraction never actually ran: every
	// captured session was rule-based, which on conversational text stores
	// fragments of narration as beliefs and extracts no entities at all.
	//
	// Nothing surfaced it because every layer behaved "correctly": the request
	// was accepted, the response was valid JSON, the parse failure had a
	// designed fallback, and the fallback produced claims. Only the CONTENT was
	// wrong, and nothing compares content.
	for _, m := range messages {
		if m.Role == RoleSystem && strings.Contains(m.Content, "JSON") && !wantsJSONArray(m.Content) {
			reqBody.ResponseFormat = &openAIResponseFmt{Type: "json_object"}
			break
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return Response{}, fmt.Errorf("marshal %s request: %w", c.provider, err)
	}

	// All supported targets (OpenAI, Ollama, openai-compat backends)
	// expose /v1/chat/completions. Ollama added the OpenAI-compat path
	// in v0.1.30 (mid-2024); the legacy /api/chat route used a different
	// response shape and defaulted to streaming, which produced
	// concatenated JSON objects and the misleading "invalid character
	// '{' after top-level value" parse error users hit on ollama.
	endpoint := c.baseURL + "/v1/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("create %s request: %w", c.provider, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%s request failed: %w", c.provider, err)
	}
	defer closeBody(resp.Body)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read %s response: %w", c.provider, err)
	}

	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("%s API returned status %d: %s", c.provider, resp.StatusCode, string(respBody))
	}

	// Use a streaming Decoder rather than Unmarshal so a stray trailing
	// frame (some Ollama versions emit one even with stream=false) does
	// not fail the whole call. We only need the first JSON object.
	var result openAIResponse
	if err := json.NewDecoder(bytes.NewReader(respBody)).Decode(&result); err != nil {
		return Response{}, fmt.Errorf("unmarshal %s response: %w", c.provider, err)
	}

	if result.Error != nil {
		return Response{}, fmt.Errorf("%s error: %s: %s", c.provider, result.Error.Type, result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return Response{}, fmt.Errorf("%s returned no choices", c.provider)
	}

	return Response{
		Content:      result.Choices[0].Message.Content,
		Model:        result.Model,
		InputTokens:  result.Usage.PromptTokens,
		OutputTokens: result.Usage.CompletionTokens,
	}, nil
}
