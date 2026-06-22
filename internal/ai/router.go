package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/stremio-ai-search/internal/ai/providers"
	"github.com/stremio-ai-search/internal/cache"
	"github.com/stremio-ai-search/internal/models"
	"github.com/stremio-ai-search/internal/singleflight"
)

// Router orchestrates multiple AI providers with intelligent failover
type Router struct {
	providers  []providers.AIProvider
	statusMap  map[string]*models.ProviderStatus
	statusMu   sync.RWMutex
	queryCache *cache.LRU
	metaCache  *cache.LRU
	sf         *singleflight.Group
	maxResults int
}

// NewRouter creates a multi-provider AI router
func NewRouter(configs []models.AIProviderConfig, queryCache, metaCache *cache.LRU, maxResults int) *Router {
	r := &Router{
		providers:  make([]providers.AIProvider, 0),
		statusMap:  make(map[string]*models.ProviderStatus),
		queryCache: queryCache,
		metaCache:  metaCache,
		sf:         &singleflight.Group{},
		maxResults: maxResults,
	}

	// Pre-populate status mapping for all supported options to ensure dashboard health rendering
	allTypes := []models.AIProviderType{
		models.ProviderGroq,
		models.ProviderCerebras,
		models.ProviderGoogle,
		models.ProviderCloudflare,
		models.ProviderOpenRouter,
	}
	for _, t := range allTypes {
		r.statusMap[string(t)] = &models.ProviderStatus{
			Provider:    t,
			IsAvailable: false,
		}
	}

	r.UpdateProviders(configs)
	return r
}

// UpdateProviders re-initializes and hot-reloads the active providers in-memory
func (r *Router) UpdateProviders(configs []models.AIProviderConfig) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()

	r.providers = make([]providers.AIProvider, 0)
	for _, cfg := range configs {
		var provider providers.AIProvider
		switch cfg.Type {
		case models.ProviderOpenRouter:
			provider = providers.NewOpenRouterProvider(cfg)
		case models.ProviderGroq:
			provider = providers.NewGroqProvider(cfg)
		case models.ProviderCerebras:
			provider = providers.NewCerebrasProvider(cfg)
		case models.ProviderGoogle:
			provider = providers.NewGoogleProvider(cfg)
		case models.ProviderCloudflare:
			provider = providers.NewCloudflareProvider(cfg)
		default:
			log.Printf("[WARN] Unknown provider type: %s", cfg.Type)
			continue
		}
		r.providers = append(r.providers, provider)
		
		if _, ok := r.statusMap[provider.Name()]; !ok {
			r.statusMap[provider.Name()] = &models.ProviderStatus{
				Provider:    cfg.Type,
				IsAvailable: true,
			}
		} else {
			r.statusMap[provider.Name()].IsAvailable = true
		}
		log.Printf("[ROUTER] Hot-reloaded provider: %s (priority %d)", provider.Name(), cfg.Priority)
	}
}

// TestProvider checks connectivity to a specific provider by sending a lightweight prompt
func (r *Router) TestProvider(ctx context.Context, name string) (int64, error) {
	r.statusMu.RLock()
	var target providers.AIProvider
	for _, p := range r.providers {
		if p.Name() == name {
			target = p
			break
		}
	}
	r.statusMu.RUnlock()

	if target == nil {
		return 0, fmt.Errorf("provider %s is not configured or enabled", name)
	}

	modelsList := target.GetModels()
	if len(modelsList) == 0 {
		return 0, fmt.Errorf("no models configured for %s", name)
	}

	// Dynamic lightweight ping using minimum possible tokens to preserve limits
	req := models.UnifiedChatRequest{
		Model: modelsList[0],
		Messages: []models.Message{
			{Role: "user", Content: "Respond with the word 'ok' and nothing else."},
		},
		MaxTokens:   5,
		Temperature: 0.1,
	}

	start := time.Now()
	_, err := target.ChatCompletion(ctx, req)
	if err != nil {
		return 0, err
	}

	return time.Since(start).Milliseconds(), nil
}

// SearchMovies sends a query to AI with provider failover and deduplication
func (r *Router) SearchMovies(ctx context.Context, query models.SearchQuery) ([]models.AIMovieResult, error) {
	if query.Clean == "" {
		return []models.AIMovieResult{}, nil
	}

	// Fixed: Unified cacheKey collapses parallel movie/series requests into a single in-memory item
	cacheKey := fmt.Sprintf("q:unified:%t:%t:%s", query.FilterAdult, query.FilterAnime, query.Clean)

	// Check cache first
	if cached, ok := r.queryCache.Get(cacheKey); ok {
		log.Printf("[CACHE HIT] %s query: %s", query.MediaType, query.Clean)
		return cached.([]models.AIMovieResult), nil
	}

	log.Printf("[CACHE MISS] %s query: %s (intent=%d)", query.MediaType, query.Clean, query.Intent)

	// Use singleflight to deduplicate concurrent identical queries
	result, err := r.sf.Do(cacheKey, func() (interface{}, error) {
		return r.executeSearch(ctx, query)
	})

	if err != nil {
		log.Printf("[ERROR] All configured providers failed to return results for search query %q: %v", query.Clean, err)
		return nil, err
	}

	results := result.([]models.AIMovieResult)

	// Cache successful results
	if len(results) > 0 {
		r.queryCache.Set(cacheKey, results)
	}

	return results, nil
}

func (r *Router) executeSearch(ctx context.Context, query models.SearchQuery) ([]models.AIMovieResult, error) {
	// Build prompt based on media type and intent
	prompt := r.buildPrompt(query)

	// Build JSON schema for structured output
	schema := buildJSONSchema(query.MediaType)

	req := models.UnifiedChatRequest{
		Messages: []models.Message{
			{Role: "system", Content: r.buildSystemPrompt(query.MediaType, query.FilterAdult, query.FilterAnime)},
			{Role: "user", Content: prompt},
		},
		ResponseFormat: &models.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &models.JSONSchema{
				Name:   "movie_search_results",
				Strict: true,
				Schema: schema,
			},
		},
		MaxTokens:   2000,
		Temperature: 0.2,
		TopP:        0.9,
	}

	// Try each provider in priority order
	var lastErr error
	for _, provider := range r.providers {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if !r.isProviderAvailable(provider.Name()) {
			log.Printf("[PROVIDER SKIP] %s is unavailable", provider.Name())
			continue
		}

		// Try each model for this provider
		for _, model := range provider.GetModels() {
			req.Model = model

			log.Printf("[PROVIDER TRY] %s / %s for %s query: %s", provider.Name(), model, query.MediaType, query.Clean)

			resp, err := provider.ChatCompletion(ctx, req)
			if err == nil {
				results := r.parseAndValidate(resp.Content, query.MediaType)
				if len(results) > 0 {
					r.recordSuccess(provider.Name())
					log.Printf("[SUCCESS] Provider=%s Model=%s Results=%d Latency=%dms", provider.Name(), model, len(results), resp.LatencyMs)
					return results, nil
				}
				// Fixed: Assigns validation failure explanation to err so it doesn't log <nil>
				err = fmt.Errorf("provider returned empty or invalid JSON schema results")
				log.Printf("[VALIDATION FAIL] Provider=%s returned no valid results", provider.Name())
			}

			lastErr = err
			r.recordFailure(provider.Name(), err)
			log.Printf("[MODEL FAIL] %s/%s: %v", provider.Name(), model, err)

			if isRateLimitError(err) {
				r.markRateLimited(provider.Name(), 60)
				break // Don't try other models from same provider
			}
		}
	}

	return nil, fmt.Errorf("all providers exhausted, last error: %w", lastErr)
}

func (r *Router) buildSystemPrompt(mediaType string, filterAdult, filterAnime bool) string {
	_ = mediaType // Parameter preserved for interface consistency

	// Dynamic Instruction Compilation to enforce strict background constraints
	var constraints []string
	if filterAdult {
		constraints = append(constraints, "EXCLUDE any adult-only, pornographic, highly explicit, NSFW, or hentai content. Only return mainstream rated titles (G, PG, PG-13, R, or TV equivalents).")
	}
	if filterAnime {
		constraints = append(constraints, "EXCLUDE any anime, manga, cartoons, or animated series/movies. Only return live-action titles.")
	}

	var constraintStr string
	if len(constraints) > 0 {
		constraintStr = "\nFILTER CONSTRAINTS:\n"
		for i, c := range constraints {
			constraintStr += fmt.Sprintf("%d. %s\n", i+1, c)
		}
	}

	// Updated: Unified System prompt instructs the AI to treat both movies and series with identical curation standards
	return fmt.Sprintf(`You are an expert movie and TV series database curator with access to the complete IMDb database.

CRITICAL RULES:
1. ONLY return movies and series that ACTUALLY EXIST
2. NEVER hallucinate titles, years, or descriptions
3. If uncertain about any entry, OMIT it completely
4. Years must be between 1888 and 2026
5. Return results ordered by relevance to the query
6. Maximum 10 results per response
%s
OUTPUT FORMAT:
Return valid JSON matching the provided schema exactly.`, constraintStr)
}

func (r *Router) buildPrompt(query models.SearchQuery) string {
	yearHint := ""
	if query.YearHint > 0 {
		yearHint = fmt.Sprintf(" Focus on %d if relevant.", query.YearHint)
	}

	// Fixed: Prompt explicitly instructs the model on key names, data types, and values to guarantee schema compliance across all JSON Mode models
	return fmt.Sprintf(`Find highly relevant movies and TV series matching this description: "%s"%s

Return up to %d total results as a JSON array named "results". Each entry must use these exact JSON key names:
- "title" (exact title string)
- "year" (integer release year)
- "reason" (brief string explaining why it matches)
- "type" (set to "movie" or "series" string depending on what it is)

Respond ONLY with valid JSON. No markdown, no explanations outside JSON.`, query.Raw, yearHint, r.maxResults*2)
}

func buildJSONSchema(mediaType string) map[string]interface{} {
	_ = mediaType // Parameter preserved for interface consistency

	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"results": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"title": map[string]interface{}{
							"type":        "string",
							"description": "Exact title as listed on IMDb",
						},
						"year": map[string]interface{}{
							"type":        "integer",
							"minimum":     1888,
							"maximum":     2026,
							"description": "Release year",
						},
						"reason": map[string]interface{}{
							"type":        "string",
							"description": "Why this matches the query",
						},
						"type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"movie", "series"},
							"description": "Media type (must be set to 'movie' or 'series' accurately)",
						},
					},
					"required":             []string{"title", "year", "reason", "type"}, // Removed "imdb_id" from required constraints
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"results"},
		"additionalProperties": false,
	}
}

func (r *Router) parseAndValidate(content, mediaType string) []models.AIMovieResult {
	_ = mediaType // Parameter preserved for interface consistency

	var parsed struct {
		Results []models.AIMovieResult `json:"results"`
	}

	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		// Try alternative key "movies"
		var altParsed struct {
			Movies []models.AIMovieResult `json:"movies"`
		}
		if err := json.Unmarshal([]byte(content), &altParsed); err != nil {
			log.Printf("[PARSE FAIL] %v", err)
			return nil
		}
		parsed.Results = altParsed.Movies
	}

	valid := make([]models.AIMovieResult, 0, len(parsed.Results))
	seen := make(map[string]bool)

	for _, result := range parsed.Results {
		// Validate title
		if strings.TrimSpace(result.Title) == "" {
			continue
		}

		// Validate year
		if result.Year < 1888 || result.Year > 2026 {
			continue
		}

		// Ensure reason exists
		if strings.TrimSpace(result.Reason) == "" {
			result.Reason = "Matches search criteria"
		}

		// Ensure type is validated
		if result.Type != "movie" && result.Type != "series" {
			result.Type = "movie" // general fallback
		}

		valid = append(valid, result)
	}

	return valid
}

// --- Provider Health Management ---

func (r *Router) isProviderAvailable(name string) bool {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	status, ok := r.statusMap[name]
	if !ok {
		return true
	}
	if !status.IsAvailable {
		if time.Now().After(status.RateLimitedUntil) {
			return true
		}
		return false
	}
	return true
}

func (r *Router) markRateLimited(name string, seconds int64) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if status, ok := r.statusMap[name]; ok {
		status.IsAvailable = false
		status.RateLimitedUntil = time.Now().Add(time.Duration(seconds) * time.Second)
	}
}

func (r *Router) recordSuccess(name string) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if status, ok := r.statusMap[name]; ok {
		status.SuccessCount++
		status.FailureCount = 0
		status.LastUsed = time.Now()
		status.IsAvailable = true
	}
}

func (r *Router) recordFailure(name string, err error) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if status, ok := r.statusMap[name]; ok {
		status.FailureCount++
		status.LastUsed = time.Now()
		if status.FailureCount >= 5 {
			status.IsAvailable = false
			status.RateLimitedUntil = time.Now().Add(5 * time.Minute)
		}
	}
}

func (r *Router) GetProviderStatus() map[string]*models.ProviderStatus {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	result := make(map[string]*models.ProviderStatus)
	for k, v := range r.statusMap {
		result[k] = v
	}
	return result
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate limited")
}
