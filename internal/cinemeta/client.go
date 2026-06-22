package cinemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stremio-ai-search/internal/cache"
	"github.com/stremio-ai-search/internal/models"
)

const (
	cinemetaBase   = "https://cinemeta-live.strem.io"
	movieEndpoint  = "/meta/movie/%s.json"
	seriesEndpoint = "/meta/series/%s.json"
)

type Client struct {
	httpClient *http.Client
	cache      *cache.LRU
}

func NewClient(metaCache *cache.LRU) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		cache: metaCache,
	}
}

// GetMetaByID fetches metadata from Cinemeta by IMDb ID and media type
func (c *Client) GetMetaByID(ctx context.Context, imdbID, mediaType string) (*models.MetaPreviewItem, error) {
	if imdbID == "" {
		return nil, fmt.Errorf("empty imdbID")
	}

	cacheKey := fmt.Sprintf("meta:%s:%s", mediaType, imdbID)
	if cached, ok := c.cache.Get(cacheKey); ok {
		return cached.(*models.MetaPreviewItem), nil
	}

	// Choose endpoint based on media type
	endpoint := movieEndpoint
	if mediaType == "series" {
		endpoint = seriesEndpoint
	}

	urlStr := fmt.Sprintf(cinemetaBase+endpoint, imdbID)
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cinemeta request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cinemeta status %d for %s", resp.StatusCode, imdbID)
	}

	var result models.MetaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("cinemeta decode: %w", err)
	}

	// Ensure required fields
	if result.Meta.ID == "" {
		result.Meta.ID = imdbID
	}
	if result.Meta.Type == "" {
		result.Meta.Type = mediaType
	}

	c.cache.Set(cacheKey, &result.Meta)
	return &result.Meta, nil
}

// GetMetaByIDBatch fetches metadata for multiple IDs concurrently
func (c *Client) GetMetaByIDBatch(ctx context.Context, results []models.AIMovieResult) map[string]*models.MetaPreviewItem {
	metaMap := make(map[string]*models.MetaPreviewItem, len(results))

	type result struct {
		id        string
		meta      *models.MetaPreviewItem
		mediaType string
		err       error
	}

	resultChan := make(chan result, len(results))

	for _, r := range results {
		go func(res models.AIMovieResult) {
			meta, err := c.GetMetaByID(ctx, res.IMDbID, res.Type)
			resultChan <- result{
				id:        res.IMDbID,
				meta:      meta,
				mediaType: res.Type,
				err:       err,
			}
		}(r)
	}

	for i := 0; i < len(results); i++ {
		r := <-resultChan
		if r.err == nil && r.meta != nil {
			metaMap[r.id] = r.meta
		}
	}

	return metaMap
}

// SearchByTitle searches Cinemeta catalog directly for exact title matches
func (c *Client) SearchByTitle(ctx context.Context, title string, mediaType string) ([]models.MetaPreviewItem, error) {
	endpoint := "/catalog/movie/top/search=%s.json"
	if mediaType == "series" {
		endpoint = "/catalog/series/top/search=%s.json"
	}
	urlStr := fmt.Sprintf(cinemetaBase+endpoint, url.QueryEscape(title))
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cinemeta search status %d", resp.StatusCode)
	}

	var catalog models.CatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, err
	}

	return catalog.Metas, nil
}

// ResolveIMDbID searches Cinemeta dynamically to resolve the exact, verified IMDb ID of a recommended title
func (c *Client) ResolveIMDbID(ctx context.Context, title string, year int, mediaType string) (string, error) {
	metas, err := c.SearchByTitle(ctx, title, mediaType)
	if err != nil {
		return "", err
	}
	if len(metas) == 0 {
		return "", fmt.Errorf("no matches found on cinemeta")
	}

	// 1. First pass: Find an item with the exact year match to resolve re-makes
	for _, meta := range metas {
		yearStr := fmt.Sprintf("%d", year)
		if strings.Contains(meta.ReleaseInfo, yearStr) {
			return meta.ID, nil
		}
	}

	// 2. Second pass: Fall back to the first search match if year mismatch is minor
	return metas[0].ID, nil
}

// BuildMetaPreview creates a minimal MetaPreviewItem from AI results
func BuildMetaPreview(result models.AIMovieResult) models.MetaPreviewItem {
	item := models.MetaPreviewItem{
		ID:          result.IMDbID,
		Type:        result.Type,
		Name:        result.Title,
		ReleaseInfo: fmt.Sprintf("%d", result.Year),
		Description: result.Reason,
		Poster:      fmt.Sprintf("https://live.metahub.space/poster/small/%s/img", result.IMDbID),
	}

	// Fallback chain for poster
	if item.Poster == "" {
		item.Poster = fmt.Sprintf("https://img.omdbapi.com/?i=%s&h=600", result.IMDbID)
	}

	return item
}

// BuildPosterFallbackChain returns poster URLs in priority order
func BuildPosterFallbackChain(imdbID string) []string {
	return []string{
		fmt.Sprintf("https://live.metahub.space/poster/small/%s/img", imdbID),
		fmt.Sprintf("https://live.metahub.space/poster/medium/%s/img", imdbID),
		fmt.Sprintf("https://images.metahub.space/poster/small/%s/img", imdbID),
	}
}
