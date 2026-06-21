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

type GoogleProvider struct {
	config     models.AIProviderConfig
	httpClient *http.Client
}

func NewGoogleProvider(config models.AIProviderConfig) *GoogleProvider {
	return &GoogleProvider{
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

func (p *GoogleProvider) Name() string {
	return "google"
}

func (p *GoogleProvider) ChatCompletion(ctx context.Context, req models.UnifiedChatRequest) (*models.UnifiedChatResponse, error) {
	start := time.Now()

	// Convert messages to Gemini format
	contents := make([]map[string]interface{}, 0, len(req.Messages))
	for _, msg := range req.Messages {
		role := msg.Role
		if role == "system" {
			role = "user"
		}
		contents = append(contents, map[string]interface{}{
			"role": role,
			"parts": []map[string]interface{}{
				{"text": msg.Content},
			},
		})
	}

	generationConfig := map[string]interface{}{
		"maxOutputTokens":  req.MaxTokens,
		"temperature":      req.Temperature,
		"topP":             req.TopP,
		"responseMimeType": "application/json", // Enforces native JSON structured output
	}

	// Dynamic reasoning/thinking budget deactivation for Gemini 2.5/3 Flash models
	if p.config.DisableThinking {
		generationConfig["thinkingConfig"] = map[string]interface{}{
			"thinkingBudget": 0, // Reduces thinking tokens allocating budget to 0
		}
	}

	geminiReq := map[string]interface{}{
		"contents":         contents,
		"generationConfig": generationConfig,
	}

	jsonBody, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent", p.config.BaseURL, req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("x-goog-api-key", p.config.APIKey)
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
	if resp.StatusCode == 403 {
		return nil, fmt.Errorf("forbidden (403) - check API key")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}

	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(geminiResp.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	content := ""
	for _, part := range geminiResp.Candidates[0].Content.Parts {
		content += part.Text
	}
	content = cleanJSONContent(content)

	return &models.UnifiedChatResponse{
		Content:      content,
		ModelUsed:    req.Model,
		Provider:     p.Name(),
		Usage: &models.Usage{
			PromptTokens:     geminiResp.UsageMetadata.PromptTokenCount,
			CompletionTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      geminiResp.UsageMetadata.TotalTokenCount,
		},
		FinishReason: geminiResp.Candidates[0].FinishReason,
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

func (p *GoogleProvider) GetModels() []string {
	return p.config.Models
}

func (p *GoogleProvider) GetMaxRPM() int {
	return p.config.MaxRPM
}

func (p *GoogleProvider) GetMaxRPD() int {
	return p.config.MaxRPD
}
