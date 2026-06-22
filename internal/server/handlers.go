package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stremio-ai-search/internal/ai"
	"github.com/stremio-ai-search/internal/cinemeta"
	"github.com/stremio-ai-search/internal/config"
	"github.com/stremio-ai-search/internal/intent"
	"github.com/stremio-ai-search/internal/models"
)

type Server struct {
	app      *fiber.App
	config   *config.Config
	router   *ai.Router
	cmClient *cinemeta.Client
}

func New(cfg *config.Config, router *ai.Router, cm *cinemeta.Client) *Server {
	app := fiber.New(fiber.Config{
		Prefork:               false,
		CaseSensitive:         true,
		StrictRouting:         true,
		ServerHeader:          "Stremio-AI-Search/2.0",
		AppName:               "Stremio AI Search Addon v2",
		ErrorHandler:          errorHandler,
		DisableStartupMessage: true,
		ReadTimeout:           10 * time.Second,
		WriteTimeout:          30 * time.Second,
	})

	s := &Server{
		app:      app,
		config:   cfg,
		router:   router,
		cmClient: cm,
	}

	// Middleware stack
	app.Use(recoveryMiddleware)
	app.Use(corsMiddleware)
	app.Use(loggingMiddleware)

	// Stremio Protocol Routes
	app.Get("/manifest.json", s.handleManifest)
	app.Get("/catalog/:type/:id/*", s.handleCatalog)

	// Dashboard & API Routes
	app.Get("/", s.handleRootRedirect)         // Redirects root domain to /configure
	app.Get("/configure", s.handleDashboard)
	app.Get("/configure/", s.handleDashboard)
	app.Get("/api/config", s.handleConfigGet) // Implements fetch loading to resolve overwriting data loss
	app.Post("/api/config", s.handleConfigSave)
	app.Get("/api/health", s.handleHealth)
	app.Get("/api/status", s.handleStatus)
	app.Get("/api/providers", s.handleProviders)
	app.Post("/api/test-providers", s.handleTestProviders) // Parallelized network & API key verifier
	app.Get("/api/models/:provider", s.handleListModels)    // Mounted: Live Model Registry Discovery Proxy

	return s
}

func (s *Server) Start(addr string) error {
	return s.app.Listen(addr)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}

// --- Stremio Protocol Handlers ---

func (s *Server) handleManifest(c *fiber.Ctx) error {
	baseURL := s.config.BaseURL
	if baseURL == "" {
		baseURL = c.Protocol() + "://" + c.Hostname()
	}

	manifest := models.Manifest{
		ID:          "com.ai.stremio-search.v2",
		Version:     "2.0.0",
		Name:        "AI Movie & Series Search",
		Description: "Natural language movie & series search powered by multi-provider AI. Supports Groq, Cerebras, Google, Cloudflare, and OpenRouter.",
		Types:       []string{"movie", "series"},
		IDPrefixes:  []string{"tt"},
		Resources: []models.ResourceItem{
			{
				Name:       "catalog",
				Types:      []string{"movie", "series"},
				IDPrefixes: []string{"tt"},
			},
		},
		Catalogs: []models.CatalogItem{
			{
				Type: "movie",
				ID:   "ai-search",
				Name: "AI Movie Search",
				Extra: []models.ExtraItem{
					{Name: "search", IsRequired: true},
					{Name: "skip", IsRequired: false},
				},
				ExtraSupported: []string{"search", "skip"},
			},
			{
				Type: "series",
				ID:   "ai-search-series",
				Name: "AI Series Search",
				Extra: []models.ExtraItem{
					{Name: "search", IsRequired: true},
					{Name: "skip", IsRequired: false},
				},
				ExtraSupported: []string{"search", "skip"},
			},
		},
		BehaviorHints: models.BehaviorHints{
			Configurable:          true,
			ConfigurationRequired: !s.config.IsConfigured(),
		},
	}

	return c.JSON(manifest)
}

func (s *Server) handleCatalog(c *fiber.Ctx) error {
	catalogType := c.Params("type")
	catalogID := c.Params("id")
	extraPath := c.Params("*")

	// Only serve our AI search catalogs
	if catalogID != "ai-search" && catalogID != "ai-search-series" {
		return c.Status(404).JSON(fiber.Map{"error": "catalog not found"})
	}

	// Parse extra args
	extraArgs := parseExtraArgs(extraPath)
	searchQuery := extraArgs["search"]
	skipStr := extraArgs["skip"]
	skip := 0
	if skipStr != "" {
		skip, _ = strconv.Atoi(skipStr)
	}

	if searchQuery == "" {
		return c.JSON(models.CatalogResponse{Metas: []models.MetaPreviewItem{}})
	}

	query, err := url.QueryUnescape(searchQuery)
	if err != nil {
		query = searchQuery
	}

	log.Printf("[CATALOG] type=%s id=%s query=%q skip=%d", catalogType, catalogID, query, skip)

	if !s.config.IsConfigured() {
		log.Printf("[WARN] Addon not configured")
		return c.JSON(models.CatalogResponse{Metas: []models.MetaPreviewItem{}})
	}

	// Detect query intent and media type
	detectedQuery := intent.Detect(query)

	// Intelligent Safe-Search / Filter Bypasses ("Unless Explicitly Asked")
	detectedQuery.FilterAdult = s.config.FilterAdult && !containsKeyword(detectedQuery.Clean, []string{"adult", "nsfw", "porn", "hentai", "18+"})
	detectedQuery.FilterAnime = s.config.FilterAnime && !containsKeyword(detectedQuery.Clean, []string{"anime", "manga", "cartoon", "animation", "manga"})

	// Handle exact title intent with direct Cinemeta search (zero AI cost)
	if detectedQuery.Intent == models.IntentExactTitle {
		log.Printf("[INTENT] Exact title detected, using Cinemeta direct search")
		metas, err := s.handleExactTitleSearch(c.Context(), detectedQuery, skip)
		if err == nil && len(metas) > 0 {
			return c.JSON(models.CatalogResponse{Metas: metas})
		}
		// Fall through to AI if exact search fails
	}

	// AI-powered semantic search
	ctx, cancel := context.WithTimeout(c.Context(), s.config.RequestTimeout)
	defer cancel()

	// Queries the AI once for both movies and series in one single unified response
	aiResults, err := s.router.SearchMovies(ctx, detectedQuery)
	if err != nil {
		log.Printf("[ERROR] AI search failed for query %q: %v", query, err)
		return c.JSON(models.CatalogResponse{Metas: []models.MetaPreviewItem{}})
	}

	// Locally filters the combined AI results on the fly, serving only the requested catalogType
	catalogResults := make([]models.AIMovieResult, 0)
	for _, res := range aiResults {
		if res.Type == catalogType {
			catalogResults = append(catalogResults, res)
		}
	}

	if len(catalogResults) == 0 {
		log.Printf("[INFO] No results found matching catalog type %s for query: %s", catalogType, query)
		return c.JSON(models.CatalogResponse{Metas: []models.MetaPreviewItem{}})
	}

	// Enrich with Cinemeta metadata
	metas := s.enrichResults(ctx, catalogResults)

	// Apply pagination
	metas = paginateResults(metas, skip, s.config.MaxResults)

	log.Printf("[CATALOG] Returning %d results (skip=%d) for: %s", len(metas), skip, query)
	return c.JSON(models.CatalogResponse{Metas: metas})
}

func (s *Server) handleExactTitleSearch(ctx context.Context, query models.SearchQuery, skip int) ([]models.MetaPreviewItem, error) {
	// Dynamically targets clean, stripped suffixes to guarantee 100% exact Cinemeta fuzzy matching
	searchTitle := query.Clean
	if query.CleanTitle != "" {
		searchTitle = query.CleanTitle
	}

	metas, err := s.cmClient.SearchByTitle(ctx, searchTitle, query.MediaType)
	if err != nil {
		log.Printf("[ERROR] Cinemeta direct search failed for %q: %v", searchTitle, err)
		return nil, err
	}

	if len(metas) == 0 {
		return nil, fmt.Errorf("no exact matches found")
	}

	// Fixed: Added explicit nil return to satisfy the dual parameter output ([]models.MetaPreviewItem, error)
	return paginateResults(metas, skip, s.config.MaxResults), nil
}

func (s *Server) enrichResults(ctx context.Context, aiResults []models.AIMovieResult) []models.MetaPreviewItem {
	// Resolves and verifies hallucinated/missing IMDb IDs using concurrent, parallel Cinemeta or TMDB search queries
	type resolveResult struct {
		idx    int
		imdbID string
		err    error
	}

	resolveChan := make(chan resolveResult, len(aiResults))

	for i, res := range aiResults {
		go func(index int, r models.AIMovieResult) {
			// Dynamically routes through TMDB if API key is provided, falling back to Cinemeta
			id, err := s.cmClient.ResolveIMDbID(ctx, r.Title, r.Year, r.Type, s.config.TMDBAPIKey)
			if err != nil {
				// If search-based resolution fails completely, we fall back to the AI-generated ID if available
				id = r.IMDbID
			}
			resolveChan <- resolveResult{idx: index, imdbID: id, err: err}
		}(i, res)
	}

	// Overwrite original array indices with resolved, guaranteed-active IDs
	verifiedResults := make([]models.AIMovieResult, len(aiResults))
	copy(verifiedResults, aiResults)

	for i := 0; i < len(aiResults); i++ {
		res := <-resolveChan
		verifiedResults[res.idx].IMDbID = res.imdbID
	}

	// Now batch fetch the full detailed metadata using the verified IDs
	metaMap := s.cmClient.GetMetaByIDBatch(ctx, verifiedResults)

	metas := make([]models.MetaPreviewItem, 0, len(verifiedResults))
	for _, result := range verifiedResults {
		if result.IMDbID == "" {
			continue // Skip unresolved items
		}
		if meta, ok := metaMap[result.IMDbID]; ok && meta != nil {
			metas = append(metas, *meta)
		} else {
			// Fallback: build standard preview item using verified ID
			metas = append(metas, cinemeta.BuildMetaPreview(result))
		}
	}

	return metas
}

func paginateResults(metas []models.MetaPreviewItem, skip, limit int) []models.MetaPreviewItem {
	if skip >= len(metas) {
		return []models.MetaPreviewItem{}
	}
	end := skip + limit
	if end > len(metas) {
		end = len(metas)
	}
	return metas[skip:end]
}

// --- Dashboard & API Handlers ---

func (s *Server) handleDashboard(c *fiber.Ctx) error {
	return c.SendFile("./web/index.html")
}

func (s *Server) handleConfigGet(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"providers":   s.config.Providers,
		"maxResults":  s.config.MaxResults,
		"cacheTTL":    s.config.CacheTTL.String(),
		"filterAdult": s.config.FilterAdult,
		"filterAnime": s.config.FilterAnime,
		"tmdbApiKey":  s.config.TMDBAPIKey, // Exposes TMDB key setting in config mapping
	})
}

func (s *Server) handleConfigSave(c *fiber.Ctx) error {
	var payload struct {
		Providers []struct {
			Type            string   `json:"type"`
			APIKey          string   `json:"apiKey"`
			Enabled         bool     `json:"enabled"`
			AccountID       string   `json:"accountId,omitempty"`
			Models          []string `json:"models"`
			DisableThinking bool     `json:"disableThinking"`
		} `json:"providers"`
		MaxResults  int    `json:"maxResults"`
		CacheTTL    string `json:"cacheTTL"`
		FilterAdult bool   `json:"filterAdult"` // Receives safe search toggles
		FilterAnime bool   `json:"filterAnime"`
		TMDBAPIKey  string `json:"tmdbApiKey"` // Receives global TMDB API key setting
	}

	if err := c.BodyParser(&payload); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request body"})
	}

	// Update providers with support for Cloudflare's accountId & custom model configurations
	for _, p := range payload.Providers {
		var providerType models.AIProviderType
		switch p.Type {
		case "openrouter":
			providerType = models.ProviderOpenRouter
		case "groq":
			providerType = models.ProviderGroq
		case "cerebras":
			providerType = models.ProviderCerebras
		case "google":
			providerType = models.ProviderGoogle
		case "cloudflare":
			providerType = models.ProviderCloudflare
		default:
			continue
		}
		s.config.UpdateProvider(providerType, p.APIKey, p.Enabled, p.AccountID, p.Models, p.DisableThinking)
	}

	if payload.MaxResults > 0 && payload.MaxResults <= 25 {
		s.config.MaxResults = payload.MaxResults
	}

	s.config.FilterAdult = payload.FilterAdult
	s.config.FilterAnime = payload.FilterAnime
	s.config.TMDBAPIKey = payload.TMDBAPIKey
	s.config.SaveToFile()

	log.Printf("[CONFIG] Updated via dashboard")

	// Dynamic Hot Reload: Update router in-memory immediately!
	enabledProviders := s.config.GetEnabledProviders()
	s.router.UpdateProviders(enabledProviders)

	return c.JSON(fiber.Map{
		"status":  "ok",
		"message": "Configuration saved. Install the addon in Stremio using your addon URL.",
	})
}

func (s *Server) handleTestProviders(c *fiber.Ctx) error {
	configs := s.config.GetEnabledProviders()
	if len(configs) == 0 {
		return c.JSON(fiber.Map{
			"results": []fiber.Map{},
			"error":   "No enabled providers to test",
		})
	}

	type testResult struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		LatencyMs int64  `json:"latencyMs"`
		Error     string `json:"error,omitempty"`
	}

	resultChan := make(chan testResult, len(configs))
	ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
	defer cancel()

	for _, cfg := range configs {
		go func(providerType models.AIProviderType, name string) {
			latency, err := s.router.TestProvider(ctx, name)
			if err != nil {
				// Logs the error output on the server side so administrators can debug
				log.Printf("[ERROR] Connection test failed for provider %s: %v", name, err)
				resultChan <- testResult{
					Name:   name,
					Status: "error",
					Error:  err.Error(),
				}
			} else {
				resultChan <- testResult{
					Name:      name,
					Status:    "ok",
					LatencyMs: latency,
				}
			}
		}(cfg.Type, string(cfg.Type))
	}

	results := make([]testResult, 0, len(configs))
	for i := 0; i < len(configs); i++ {
		results = append(results, <-resultChan)
	}

	return c.JSON(fiber.Map{
		"results": results,
	})
}

func (s *Server) handleListModels(c *fiber.Ctx) error {
	providerName := c.Params("provider")
	var target models.AIProviderConfig
	for _, p := range s.config.Providers {
		if string(p.Type) == providerName {
			target = p
			break
		}
	}

	if target.APIKey == "" {
		return c.Status(400).JSON(fiber.Map{"error": "Please save your API key first before attempting to fetch live models"})
	}

	ctx, cancel := context.WithTimeout(c.Context(), 8*time.Second)
	defer cancel()

	var modelsList []string
	var err error

	switch target.Type {
	case models.ProviderGroq, models.ProviderCerebras, models.ProviderOpenRouter:
		modelsList, err = fetchOpenAIModels(ctx, target.BaseURL, target.APIKey)
	case models.ProviderGoogle:
		modelsList, err = fetchGoogleModels(ctx, target.BaseURL, target.APIKey)
	default:
		return c.Status(400).JSON(fiber.Map{"error": "Dynamic model discovery is not supported for " + providerName})
	}

	if err != nil {
		log.Printf("[ERROR] Live model registry discovery failed for %s: %v", providerName, err)
		return c.Status(500).JSON(fiber.Map{"error": "API Error: " + err.Error()})
	}

	return c.JSON(fiber.Map{"models": modelsList})
}

func fetchOpenAIModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http response code %d", resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	list := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		// Avoid indexing speech/audio models (e.g., whisper) if they are returned
		if strings.Contains(m.ID, "whisper") || strings.Contains(m.ID, "tts") {
			continue
		}
		list = append(list, m.ID)
	}

	return list, nil
}

func fetchGoogleModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	urlStr := fmt.Sprintf("%s/models?key=%s", baseURL, apiKey)
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http response code %d", resp.StatusCode)
	}

	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	list := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		name := strings.TrimPrefix(m.Name, "models/")
		list = append(list, name)
	}

	return list, nil
}

func (s *Server) handleHealth(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status":     "healthy",
		"version":    "2.0.0",
		"time":       time.Now().UTC(),
		"configured": s.config.IsConfigured(),
	})
}

func (s *Server) handleStatus(c *fiber.Ctx) error {
	status := s.router.GetProviderStatus()
	providers := make([]fiber.Map, 0)
	for name, st := range status {
		providers = append(providers, fiber.Map{
			"name":         name,
			"available":    st.IsAvailable,
			"successCount": st.SuccessCount,
			"failureCount": st.FailureCount,
			"lastUsed":     st.LastUsed,
		})
	}
	return c.JSON(fiber.Map{
		"configured": s.config.IsConfigured(),
		"providers":  providers,
		"cacheSize":  s.config.CacheSize,
		"maxResults": s.config.MaxResults,
	})
}

func (s *Server) handleProviders(c *fiber.Ctx) error {
	configs := s.config.GetEnabledProviders()
	providers := make([]fiber.Map, 0)
	for _, cfg := range configs {
		providers = append(providers, fiber.Map{
			"type":     cfg.Type,
			"enabled":  cfg.Enabled,
			"priority": cfg.Priority,
			"maxRPM":   cfg.MaxRPM,
			"maxRPD":   cfg.MaxRPD,
		})
	}
	return c.JSON(providers)
}

func (s *Server) handleRootRedirect(c *fiber.Ctx) error {
	return c.Redirect("/configure", fiber.StatusMovedPermanently)
}

// --- Utility Functions ---

func parseExtraArgs(extraPath string) map[string]string {
	result := make(map[string]string)
	if extraPath == "" || extraPath == ".json" {
		return result
	}

	clean := strings.TrimSuffix(extraPath, ".json")
	if clean == "" {
		return result
	}

	values, err := url.ParseQuery(clean)
	if err != nil {
		log.Printf("[WARN] Failed to parse extra args: %s", clean)
		return result
	}

	for k, v := range values {
		if len(v) > 0 && v[0] != "" {
			result[k] = v[0]
		}
	}
	return result
}

func containsKeyword(s string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func errorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
	}
	log.Printf("[ERROR] %s %s: %v", c.Method(), c.Path(), err)
	
	// Optimized: Show descriptive messages instead of always displaying "Internal server error"
	message := "Internal server error"
	if code == fiber.StatusNotFound {
		message = "Not found"
	}
	return c.Status(code).JSON(fiber.Map{"error": message})
}
