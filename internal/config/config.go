package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stremio-ai-search/internal/models"
)

type Config struct {
	Port             string                    `json:"port"`
	Providers        []models.AIProviderConfig `json:"providers"`
	CacheSize        int                       `json:"cache_size"`
	CacheTTL         time.Duration             `json:"cache_ttl"`
	LogLevel         string                    `json:"log_level"`
	MaxResults       int                       `json:"max_results"`
	RequestTimeout   time.Duration             `json:"request_timeout"`
	ModelTimeout     time.Duration             `json:"model_timeout"`
	MaxRetries       int                       `json:"max_retries"`
	DashboardEnabled bool                      `json:"dashboard_enabled"`
	BaseURL          string                    `json:"base_url"`
	ConfigFile       string                    `json:"-"`
}

func Load() *Config {
	cfg := &Config{
		Port:             getEnv("PORT", "8080"),
		CacheSize:        parseInt(getEnv("CACHE_SIZE", "1000")),
		CacheTTL:         parseDuration(getEnv("CACHE_TTL", "1h")),
		LogLevel:         getEnv("LOG_LEVEL", "info"),
		MaxResults:       parseInt(getEnv("MAX_RESULTS", "10")),
		RequestTimeout:   parseDuration(getEnv("REQUEST_TIMEOUT", "30s")),
		ModelTimeout:     parseDuration(getEnv("MODEL_TIMEOUT", "10s")),
		MaxRetries:       parseInt(getEnv("MAX_RETRIES", "3")),
		DashboardEnabled: parseBool(getEnv("DASHBOARD_ENABLED", "true")),
		BaseURL:          getEnv("BASE_URL", ""),
		ConfigFile:       getEnv("CONFIG_FILE", "./config.json"),
	}

	cfg.Providers = loadProviders()
	cfg.loadFromFile()
	return cfg
}

func loadProviders() []models.AIProviderConfig {
	accountID := getEnv("CLOUDFLARE_ACCOUNT_ID", "")
	return []models.AIProviderConfig{
		{
			Type:     models.ProviderGroq,
			APIKey:   getEnv("GROQ_API_KEY", ""),
			Enabled:  getEnv("GROQ_API_KEY", "") != "",
			Priority: 1, // Groq is the fastest and primary provider
			Models:   splitEnv("GROQ_MODELS", "llama-3.3-70b-versatile,llama-3.1-8b-instant,gemma2-9b-it,mixtral-8x7b-32768"),
			MaxRPM:   30,
			MaxRPD:   14400,
			BaseURL:  "https://api.groq.com/openai/v1",
		},
		{
			Type:     models.ProviderCerebras,
			APIKey:   getEnv("CEREBRAS_API_KEY", ""),
			Enabled:  getEnv("CEREBRAS_API_KEY", "") != "",
			Priority: 2,
			Models:   splitEnv("CEREBRAS_MODELS", "llama3.1-8b,llama3.1-70b"),
			MaxRPM:   30,
			MaxRPD:   14400,
			BaseURL:  "https://api.cerebras.ai/v1",
		},
		{
			Type:     models.ProviderGoogle,
			APIKey:   getEnv("GOOGLE_API_KEY", ""),
			Enabled:  getEnv("GOOGLE_API_KEY", "") != "",
			Priority: 3,
			Models:   splitEnv("GOOGLE_MODELS", "gemini-1.5-flash,gemini-2.0-flash,gemini-2.5-flash"),
			MaxRPM:   15,
			MaxRPD:   1500,
			BaseURL:  "https://generativelanguage.googleapis.com/v1beta",
		},
		{
			Type:      models.ProviderCloudflare,
			APIKey:    getEnv("CLOUDFLARE_API_KEY", ""),
			Enabled:   getEnv("CLOUDFLARE_API_KEY", "") != "",
			Priority:  4,
			Models:    splitEnv("CLOUDFLARE_MODELS", "@cf/meta/llama-3.3-70b-instruct-awq,@cf/meta/llama-3-8b-instruct"),
			MaxRPM:    100,
			MaxRPD:    100000,
			BaseURL:   "https://api.cloudflare.com/client/v4/accounts/" + accountID + "/ai/run",
			AccountID: accountID,
		},
		{
			Type:     models.ProviderOpenRouter,
			APIKey:   getEnv("OPENROUTER_API_KEY", ""),
			Enabled:  getEnv("OPENROUTER_API_KEY", "") != "",
			Priority: 5,
			Models:   splitEnv("OPENROUTER_MODELS", "meta-llama/llama-3-8b-instruct:free,google/gemma-2-9b-it:free"),
			MaxRPM:   20,
			MaxRPD:   50,
			BaseURL:  "https://openrouter.ai/api/v1",
		},
	}
}

func (c *Config) GetEnabledProviders() []models.AIProviderConfig {
	enabled := make([]models.AIProviderConfig, 0)
	for _, p := range c.Providers {
		if p.Enabled && p.APIKey != "" {
			enabled = append(enabled, p)
		}
	}
	// Sort by priority ascending (1 is primary)
	for i := 0; i < len(enabled); i++ {
		for j := i + 1; j < len(enabled); j++ {
			if enabled[j].Priority < enabled[i].Priority {
				enabled[i], enabled[j] = enabled[j], enabled[i]
			}
		}
	}
	return enabled
}

func (c *Config) IsConfigured() bool {
	for _, p := range c.Providers {
		if p.Enabled && p.APIKey != "" {
			return true
		}
	}
	return false
}

func (c *Config) SaveToFile() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.ConfigFile, data, 0644)
}

func (c *Config) loadFromFile() {
	data, err := os.ReadFile(c.ConfigFile)
	if err != nil {
		return
	}
	var fileCfg Config
	if err := json.Unmarshal(data, &fileCfg); err != nil {
		return
	}
	// Fixed: Only overwrite values if they are explicitly configured in the JSON file
	if fileCfg.Port != "" {
		c.Port = fileCfg.Port
	}
	// Safely overlays file configuration over existing environment settings
	if len(fileCfg.Providers) > 0 {
		for _, fp := range fileCfg.Providers {
			for i, p := range c.Providers {
				if p.Type == fp.Type {
					if c.Providers[i].APIKey == "" {
						c.Providers[i].APIKey = fp.APIKey
						c.Providers[i].Enabled = fp.Enabled
						if len(fp.Models) > 0 {
							c.Providers[i].Models = fp.Models
						}
						if fp.Type == models.ProviderCloudflare {
							c.Providers[i].AccountID = fp.AccountID
							if fp.AccountID != "" {
								c.Providers[i].BaseURL = "https://api.cloudflare.com/client/v4/accounts/" + fp.AccountID + "/ai/run"
							}
						}
					} else {
						// Ensure in-memory sync for dashboard configuration modifications
						c.Providers[i].Enabled = fp.Enabled
						if len(fp.Models) > 0 {
							c.Providers[i].Models = fp.Models
						}
					}
				}
			}
		}
	}
	if fileCfg.MaxResults > 0 {
		c.MaxResults = fileCfg.MaxResults
	}
	if fileCfg.CacheTTL > 0 {
		c.CacheTTL = fileCfg.CacheTTL
	}
}

func (c *Config) UpdateProvider(providerType models.AIProviderType, apiKey string, enabled bool, accountID string, modelsList []string) {
	for i := range c.Providers {
		if c.Providers[i].Type == providerType {
			c.Providers[i].APIKey = apiKey
			c.Providers[i].Enabled = enabled
			if len(modelsList) > 0 {
				c.Providers[i].Models = modelsList
			}
			if providerType == models.ProviderCloudflare {
				c.Providers[i].AccountID = accountID
				if accountID != "" {
					c.Providers[i].BaseURL = "https://api.cloudflare.com/client/v4/accounts/" + accountID + "/ai/run"
				}
			}
			c.SaveToFile()
			return
		}
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func splitEnv(key, defaultVal string) []string {
	v := getEnv(key, defaultVal)
	if v == "" {
		return []string{}
	}
	parts := strings.Split(v, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseInt(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Hour
	}
	return d
}

func parseBool(s string) bool {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false
	}
	return v
}
