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

func (r *Router) buildPromptNoResults(query models.SearchQuery) string {
	return r.buildPrompt(query)
}

func (r *Router) isProviderAvailablePublic(name string) bool {
	return r.isProviderAvailable(name)
}

func (r *Router) getProvidersCount() int {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	return len(r.providers)
}

func (r *Router) getStatusMap() map[string]*models.ProviderStatus {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	return r.statusMap
}

func (r *Router) getProviders() []providers.AIProvider {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	return r.providers
}

func (r *Router) getQueryCache() *cache.LRU {
	return r.queryCache
}

func (r *Router) getMetaCache() *cache.LRU {
	return r.metaCache
}

func (r *Router) getSf() *singleflight.Group {
	return r.sf
}

func (r *Router) getMaxResults() int {
	return r.maxResults
}

func (r *Router) setMaxResults(val int) {
	r.maxResults = val
}

func (r *Router) setProviders(val []providers.AIProvider) {
	r.providers = val
}

func (r *Router) setStatusMap(val map[string]*models.ProviderStatus) {
	r.statusMap = val
}

func (r *Router) setQueryCache(val *cache.LRU) {
	r.queryCache = val
}

func (r *Router) setMetaCache(val *cache.LRU) {
	r.metaCache = val
}

func (r *Router) setSf(val *singleflight.Group) {
	r.sf = val
}
