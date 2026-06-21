package models

import (
	"encoding/json"
	"time"
)

// AIProviderType represents supported AI inference providers
type AIProviderType string

const (
	ProviderOpenRouter AIProviderType = "openrouter"
	ProviderGroq       AIProviderType = "groq"
	ProviderCerebras   AIProviderType = "cerebras"
	ProviderGoogle     AIProviderType = "google"
	ProviderCloudflare AIProviderType = "cloudflare"
)

// AIProviderConfig holds configuration for a single provider
type AIProviderConfig struct {
	Type            AIProviderType `json:"type"`
	APIKey          string         `json:"apiKey"`
	Enabled         bool           `json:"enabled"`
	Models          []string       `json:"models"`
	Priority        int            `json:"priority"`
	MaxRPM          int            `json:"maxRPM"`
	MaxRPD          int            `json:"maxRPD"`
	BaseURL         string         `json:"baseURL"`
	AccountID       string         `json:"accountId,omitempty"`       // Retained for Cloudflare persistence
	DisableThinking bool           `json:"disableThinking,omitempty"` // Additive: Suppression flag for reasoning models
}

// UnifiedChatRequest is the normalized request sent to any provider
type UnifiedChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	TopP           float64         `json:"top_p,omitempty"`
}

// UnifiedChatResponse is the normalized response from any provider
type UnifiedChatResponse struct {
	Content      string `json:"content"`
	ModelUsed    string `json:"model_used"`
	Provider     string `json:"provider"`
	Usage        *Usage `json:"usage,omitempty"`
	FinishReason string `json:"finish_reason"`
	LatencyMs    int64  `json:"latency_ms"`
}

// AIMovieResult is the structured output from AI
type AIMovieResult struct {
	Title  string `json:"title"`
	Year   int    `json:"year"`
	IMDbID string `json:"imdb_id"`
	Reason string `json:"reason"`
	Type   string `json:"type,omitempty"`
}

// UnmarshalJSON implements custom JSON unmarshaling to support key aliases for non-schema-enforced LLM outputs
func (r *AIMovieResult) UnmarshalJSON(data []byte) error {
	type Alias AIMovieResult
	aux := &struct {
		IMDbIDAlias1 string `json:"imdbId"`
		IMDbIDAlias2 string `json:"imdbID"`
		IMDbIDAlias3 string `json:"imdb"`
		IMDbIDAlias4 string `json:"id"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Auto-corrects key mapping discrepancies
	if r.IMDbID == "" {
		if aux.IMDbIDAlias1 != "" {
			r.IMDbID = aux.IMDbIDAlias1
		} else if aux.IMDbIDAlias2 != "" {
			r.IMDbID = aux.IMDbIDAlias2
		} else if aux.IMDbIDAlias3 != "" {
			r.IMDbID = aux.IMDbIDAlias3
		} else if aux.IMDbIDAlias4 != "" {
			r.IMDbID = aux.IMDbIDAlias4
		}
	}

	return nil
}

type AIResponse struct {
	Movies []AIMovieResult `json:"movies"`
	Series []AIMovieResult `json:"series,omitempty"`
}

// ProviderStatus tracks health metrics for each provider
type ProviderStatus struct {
	Provider         AIProviderType
	LastUsed         time.Time
	FailureCount     int
	SuccessCount     int
	IsAvailable      bool
	RateLimitedUntil time.Time
	AvgLatency       time.Duration
}

// QueryIntent represents the detected intent of a user query
type QueryIntent int

const (
	IntentExactTitle QueryIntent = iota
	IntentSemantic
	IntentSimilar
	IntentGenre
	IntentActor
	IntentDirector
)

// SearchQuery represents a parsed and analyzed user query
type SearchQuery struct {
	Raw         string      `json:"raw"`
	Clean       string      `json:"clean"`
	Intent      QueryIntent `json:"intent"`
	MediaType   string      `json:"media_type"`
	YearHint    int         `json:"year_hint,omitempty"`
	FilterAdult bool        `json:"filter_adult,omitempty"` // Additive: Dynamic NSFW filter flag
	FilterAnime bool        `json:"filter_anime,omitempty"` // Additive: Dynamic Anime filter flag
	CleanTitle  string      `json:"clean_title,omitempty"`  // Additive: Suffix-stripped title for Cinemeta exact matching
}
