package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/stremio-ai-search/internal/models"
)

type GroqProvider struct {
	config     models.AIProviderConfig
	httpClient *http.Client
}

func NewGroqProvider(config models.AIProviderConfig) *GroqProvider {
	return &GroqProvider{
		config: config,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *GroqProvider) Name() string {
	return "groq"
}

func (p *GroqProvider) ChatCompletion(ctx context.Context, req models.UnifiedChatRequest) (*models.UnifiedChatResponse, error) {
	start := time.Now()

	groqReq := struct {
		Model           string                 `json:"model"`
		Messages        []models.Message       `json:"messages"`
		ResponseFormat  map[string]interface{} `json:"response_format,omitempty"`
		MaxTokens       int                    `json:"max_tokens,omitempty"`
		Temperature     float64                `json:"temperature,omitempty"`
		TopP            float64                `json:"top_p,omitempty"`
		ReasoningFormat string                 `json:"reasoning_format,omitempty"` // Additive: format controls
		ReasoningEffort string                 `json:"reasoning_effort,omitempty"` // Additive: effort levels
	}{
		Model:       req.Model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if req.ResponseFormat != nil {
		groqReq.ResponseFormat = map[string]interface{}{
			"type": "json_object",
		}
	}

	// Dynamic reasoning format suppression for Groq reasoning models (e.g. Qwen 3.6 27B / GPT-OSS)
	if p.config.DisableThinking {
		groqReq.ReasoningFormat = "hidden"
		groqReq.ReasoningEffort = "none"
	}

	jsonBody, err := json.Marshal(groqReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.config.BaseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("unauthorized (401)")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var groqResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &groqResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if groqResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", groqResp.Error.Message)
	}

	if len(groqResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	content := cleanJSONContent(groqResp.Choices[0].Message.Content)

	return &models.UnifiedChatResponse{
		Content:      content,
		ModelUsed:    groqResp.Model,
		Provider:     p.Name(),
		Usage: &models.Usage{
			PromptTokens:     groqResp.Usage.PromptTokens,
			CompletionTokens: groqResp.Usage.CompletionTokens,
			TotalTokens:      groqResp.Usage.TotalTokens,
		},
		FinishReason: groqResp.Choices[0].FinishReason,
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

func (p *GroqProvider) GetModels() []string {
	return p.config.Models
}

func (p *GroqProvider) GetMaxRPM() int {
	return p.config.MaxRPM
}

func (p *GroqProvider) GetMaxRPD() int {
	return p.config.MaxRPD
}
