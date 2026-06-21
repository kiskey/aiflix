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

type CerebrasProvider struct {
	config     models.AIProviderConfig
	httpClient *http.Client
}

func NewCerebrasProvider(config models.AIProviderConfig) *CerebrasProvider {
	return &CerebrasProvider{
		config: config,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *CerebrasProvider) Name() string {
	return "cerebras"
}

func (p *CerebrasProvider) ChatCompletion(ctx context.Context, req models.UnifiedChatRequest) (*models.UnifiedChatResponse, error) {
	start := time.Now()

	cbReq := struct {
		Model           string                 `json:"model"`
		Messages        []models.Message       `json:"messages"`
		ResponseFormat  map[string]interface{} `json:"response_format,omitempty"`
		MaxTokens       int                    `json:"max_tokens,omitempty"`
		Temperature     float64                `json:"temperature,omitempty"`
		TopP            float64                `json:"top_p,omitempty"`
		ReasoningEffort string                 `json:"reasoning_effort,omitempty"`
		ReasoningFormat string                 `json:"reasoning_format,omitempty"` // Additive: Reasoning format controls
	}{
		Model:       req.Model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if req.ResponseFormat != nil {
		cbReq.ResponseFormat = map[string]interface{}{
			"type": "json_object", // Triggers robust native JSON constraints on Cerebras
		}
	}

	// Dynamic reasoning suppression based on official Cerebras API specification
	if p.config.DisableThinking {
		if req.Model == "gpt-oss-120b" {
			cbReq.ReasoningFormat = "hidden" // Drops reasoning text/logprobs completely from the response
			cbReq.ReasoningEffort = "low"    // Minimizes reasoning tokens to reduce latency
		} else {
			cbReq.ReasoningEffort = "none" // Supported for Z.ai GLM series models
		}
	}

	jsonBody, err := json.Marshal(cbReq)
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

	var cbResp struct {
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
	}

	if err := json.Unmarshal(body, &cbResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(cbResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	content := cleanJSONContent(cbResp.Choices[0].Message.Content)

	return &models.UnifiedChatResponse{
		Content:      content,
		ModelUsed:    cbResp.Model,
		Provider:     p.Name(),
		Usage: &models.Usage{
			PromptTokens:     cbResp.Usage.PromptTokens,
			CompletionTokens: cbResp.Usage.CompletionTokens,
			TotalTokens:      cbResp.Usage.TotalTokens,
		},
		FinishReason: cbResp.Choices[0].FinishReason,
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

func (p *CerebrasProvider) GetModels() []string {
	return p.config.Models
}

func (p *CerebrasProvider) GetMaxRPM() int {
	return p.config.MaxRPM
}

func (p *CerebrasProvider) GetMaxRPD() int {
	return p.config.MaxRPD
}
