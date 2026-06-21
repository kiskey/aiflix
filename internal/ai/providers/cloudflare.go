package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stremio-ai-search/internal/models"
)

type CloudflareProvider struct {
	config     models.AIProviderConfig
	httpClient *http.Client
}

func NewCloudflareProvider(config models.AIProviderConfig) *CloudflareProvider {
	return &CloudflareProvider{
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

func (p *CloudflareProvider) Name() string {
	return "cloudflare"
}

func (p *CloudflareProvider) ChatCompletion(ctx context.Context, req models.UnifiedChatRequest) (*models.UnifiedChatResponse, error) {
	start := time.Now()

	// Optimized message compilation using strings.Builder to minimize heap allocations and GC pressure
	var builder strings.Builder
	for _, msg := range req.Messages {
		builder.WriteString(msg.Role)
		builder.WriteString(": ")
		builder.WriteString(msg.Content)
		builder.WriteString("\n")
	}
	prompt := builder.String()

	cfReq := map[string]interface{}{
		"prompt":      prompt,
		"max_tokens":  req.MaxTokens,
		"temperature": req.Temperature,
		"top_p":       req.TopP,
	}

	jsonBody, err := json.Marshal(cfReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/%s", p.config.BaseURL, req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
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
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var cfResp struct {
		Result struct {
			Response string `json:"response"`
		} `json:"result"`
		Slice   bool `json:"success"` // Maps cf native success response
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(body, &cfResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if !cfResp.Success {
		if len(cfResp.Errors) > 0 {
			return nil, fmt.Errorf("API error: %s", cfResp.Errors[0].Message)
		}
		return nil, fmt.Errorf("API request failed")
	}

	content := cleanJSONContent(cfResp.Result.Response)

	return &models.UnifiedChatResponse{
		Content:      content,
		ModelUsed:    req.Model,
		Provider:     p.Name(),
		Usage:        nil, // Cloudflare does not provide usage tokens stats
		FinishReason: "stop",
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

func (p *CloudflareProvider) GetModels() []string {
	return p.config.Models
}

func (p *CloudflareProvider) GetMaxRPM() int {
	return p.config.MaxRPM
}

func (p *CloudflareProvider) GetMaxRPD() int {

	return p.config.MaxRPD
}
