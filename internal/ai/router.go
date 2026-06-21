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

	// Build cache key with media type namespace
	cacheKey := fmt.Sprintf("q:%s:%s", query.MediaType, query.Clean)

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
		// Fixed: Logs the error output on the server side so administrators can debug
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
			{Role: "system", Content: r.buildSystemPrompt(query.MediaType)},
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

func (r *Router) buildSystemPrompt(mediaType string) string {
	mediaLabel := "movies"
	if mediaType == "series" {
		mediaLabel = "TV series and shows"
	}

	return fmt.Sprintf(`You are an expert %s database curator with access to the complete IMDb database.

CRITICAL RULES:
1. ONLY return %s that ACTUALLY EXIST on IMDb with verified IMDb IDs
2. NEVER hallucinate titles, years, or IMDb IDs
3. If uncertain about any entry, OMIT it completely
4. IMDb IDs must be exact format: "tt" followed by 7-10 digits (e.g., tt0111161)
5. Years must be between 1888 and 2026
6. Return results ordered by relevance to the query
7. Maximum 10 results per response

OUTPUT FORMAT:
Return valid JSON matching the provided schema exactly.`, mediaLabel, mediaLabel)
}

func (r *Router) buildPrompt(query models.SearchQuery) string {
	mediaLabel := "movies"
	if query.MediaType == "series" {
		mediaLabel = "TV series"
	}

	yearHint := ""
	if query.YearHint > 0 {
		yearHint = fmt.Sprintf(" Focus on %d if relevant.", query.YearHint)
	}

	return fmt.Sprintf(`Find %s matching this description: "%s"%s

Return up to %d %s as a JSON array. Each entry must include exact title, correct year, valid IMDb ID (tt + digits), and a brief reason why it matches.

Respond ONLY with valid JSON. No markdown, no explanations outside JSON.`, mediaLabel, query.Raw, yearHint, r.maxResults, mediaLabel)
}

func buildJSONSchema(mediaType string) map[string]interface{} {
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
						"imdb_id": map[string]interface{}{
							"type":        "string",
							"pattern":     "^tt[0-9]{7,10}$",
							"description": "IMDb ID in ttXXXXXXX format",
						},
						"reason": map[string]interface{}{
							"type":        "string",
							"description": "Why this matches the query",
						},
						"type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"movie", "series"},
							"description": "Media type",
						},
					},
					"required":             []string{"title", "year", "imdb_id", "reason"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"results"},
		"additionalProperties": false,
	}
}

func (r *Router) parseAndValidate(content, mediaType string) []models.AIMovieResult {
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
	imdbRegex := regexp.MustCompile(`^tt[0-9]{7,10}$`)

	for _, result := range parsed.Results {
		// Deduplicate by IMDb ID
		if seen[result.IMDbID] {
			continue
		}
		seen[result.IMDbID] = true

		// Validate title
		if strings.TrimSpace(result.Title) == "" {
			continue
		}

		// Validate year
		if result.Year < 1888 || result.Year > 2026 {
			continue
		}

		// Validate IMDb ID
		if !imdbRegex.MatchString(result.IMDbID) {
			continue
		}

		// Ensure reason exists
		if strings.TrimSpace(result.Reason) == "" {
			result.Reason = "Matches search criteria"
		}

		// Set media type if not provided
		if result.Type == "" {
			result.Type = mediaType
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
