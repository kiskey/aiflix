package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stremio-ai-search/internal/ai"
	"github.com/stremio-ai-search/internal/cache"
	"github.com/stremio-ai-search/internal/cinemeta"
	"github.com/stremio-ai-search/internal/config"
	"github.com/stremio-ai-search/internal/server"
)

func main() {
	log.Println("========================================")
	log.Println("  Stremio AI Search Addon v2.0.0")
	log.Println("  Multi-Provider AI Movie & Series Search")
	log.Println("========================================")

	cfg := config.Load()
	log.Printf("[CONFIG] Port=%s, CacheSize=%d, CacheTTL=%s", cfg.Port, cfg.CacheSize, cfg.CacheTTL)

	if !cfg.IsConfigured() {
		log.Println("[WARN] No AI providers configured. Add API keys via dashboard or environment variables.")
		log.Println("[INFO] Supported providers: Groq, Cerebras, Google AI Studio, Cloudflare, OpenRouter")
	}

	// Initialize caches
	queryCache := cache.NewLRU(cfg.CacheSize, cfg.CacheTTL)
	metaCache := cache.NewLRU(cfg.CacheSize*2, cfg.CacheTTL*2)
	log.Printf("[CACHE] Query cache: size=%d, TTL=%s", cfg.CacheSize, cfg.CacheTTL)
	log.Printf("[CACHE] Meta cache: size=%d, TTL=%s", cfg.CacheSize*2, cfg.CacheTTL*2)

	// Initialize Cinemeta client
	cmClient := cinemeta.NewClient(metaCache)
	log.Println("[CINEMETA] Client initialized")

	// Initialize AI router with multi-provider support
	enabledProviders := cfg.GetEnabledProviders()
	router := ai.NewRouter(enabledProviders, queryCache, metaCache, cfg.MaxResults)
	log.Printf("[AI ROUTER] Initialized with %d providers", len(enabledProviders))
	for _, p := range enabledProviders {
		log.Printf("  [PROVIDER] %s (priority=%d, maxRPD=%d)", p.Type, p.Priority, p.MaxRPD)
	}

	// Initialize HTTP server
	srv := server.New(cfg, router, cmClient)
	log.Printf("[SERVER] Configured on port %s", cfg.Port)

	// Start server
	go func() {
		addr := ":" + cfg.Port
		log.Printf("[SERVER] Listening on %s", addr)
		if err := srv.Start(addr); err != nil {
			log.Fatalf("[FATAL] Server failed to start: %v", err)
		}
	}()

	log.Println("[SERVER] Addon is running. Press Ctrl+C to stop.")
	log.Printf("[SERVER] Dashboard: http://localhost:%s/configure", cfg.Port)
	log.Printf("[SERVER] Manifest: http://localhost:%s/manifest.json", cfg.Port)
	log.Printf("[SERVER] Health: http://localhost:%s/api/health", cfg.Port)

	// Wait for interrupt
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("\n[SHUTDOWN] Graceful shutdown initiated...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[SHUTDOWN] Error: %v", err)
	}

	log.Println("[SHUTDOWN] Server stopped. Goodbye!")
}
