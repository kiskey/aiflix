package server

import (
	"context"
	"fmt"
	"log"
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
	app.Get("/", s.handleRootRedirect)         // Additive: Redirects root domain to /configure
	app.Get("/configure", s.handleDashboard)
	app.Get("/configure/", s.handleDashboard)
	app.Get("/api/config", s.handleConfigGet) // Implements fetch loading to resolve overwriting data loss
	app.Post("/api/config", s.handleConfigSave)
	app.Get("/api/health", s.handleHealth)
	app.Get("/api/status", s.handleStatus)
	app.Get("/api/providers", s.handleProviders)

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

	// Override media type from catalog if specified
	if catalogType == "movie" {
		detectedQuery.MediaType = "movie"
	} else if catalogType == "series" {
		detectedQuery.MediaType = "series"
	}

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

	aiResults, err := s.router.SearchMovies(ctx, detectedQuery)
	if err != nil {
		log.Printf("[ERROR] AI search failed: %v", err)
		return c.JSON(models.CatalogResponse{Metas: []models.MetaPreviewItem{}})
	}

	if len(aiResults) == 0 {
		log.Printf("[INFO] AI returned no results for: %s", query)
		return c.JSON(models.CatalogResponse{Metas: []models.MetaPreviewItem{}})
	}

	// Enrich with Cinemeta metadata
	metas := s.enrichResults(ctx, aiResults)

	// Apply pagination
	metas = paginateResults(metas, skip, s.config.MaxResults)

	log.Printf("[CATALOG] Returning %d results (skip=%d) for: %s", len(metas), skip, query)
	return c.JSON(models.CatalogResponse{Metas: metas})
}

func (s *Server) handleExactTitleSearch(ctx context.Context, query models.SearchQuery, skip int) ([]models.MetaPreviewItem, error) {
	// Try Cinemeta direct search first with dynamic media types
	metas, err := s.cmClient.SearchByTitle(ctx, query.Clean, query.MediaType)
	if err != nil {
		return nil, err
	}

	if len(metas) == 0 {
		return nil, fmt.Errorf("no exact matches found")
	}

	// Apply pagination
	return paginateResults(metas, skip, s.config.MaxResults), nil
}

func (s *Server) enrichResults(ctx context.Context, aiResults []models.AIMovieResult) []models.MetaPreviewItem {
	// Batch fetch from Cinemeta
	metaMap := s.cmClient.GetMetaByIDBatch(ctx, aiResults)

	metas := make([]models.MetaPreviewItem, 0, len(aiResults))
	for _, result := range aiResults {
		if meta, ok := metaMap[result.IMDbID]; ok && meta != nil {
			metas = append(metas, *meta)
		} else {
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
		"providers":  s.config.Providers,
		"maxResults": s.config.MaxResults,
		"cacheTTL":   s.config.CacheTTL.String(),
	})
}

func (s *Server) handleConfigSave(c *fiber.Ctx) error {
	var payload struct {
		Providers []struct {
			Type      string `json:"type"`
			APIKey    string `json:"apiKey"`
			Enabled   bool   `json:"enabled"`
			AccountID string `json:"accountId,omitempty"`
		} `json:"providers"`
		MaxResults int    `json:"maxResults"`
		CacheTTL   string `json:"cacheTTL"`
	}

	if err := c.BodyParser(&payload); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request body"})
	}

	// Update providers with support for Cloudflare's accountId
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
		s.config.UpdateProvider(providerType, p.APIKey, p.Enabled, p.AccountID)
	}

	if payload.MaxResults > 0 && payload.MaxResults <= 25 {
		s.config.MaxResults = payload.MaxResults
	}

	log.Printf("[CONFIG] Updated via dashboard")
	return c.JSON(fiber.Map{
		"status":  "ok",
		"message": "Configuration saved. Install the addon in Stremio using your addon URL.",
	})
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
