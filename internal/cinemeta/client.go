package cinemeta

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

// ResolveIMDbID searches Cinemeta or TMDB dynamically to resolve the exact, verified IMDb ID of a recommended title
func (c *Client) ResolveIMDbID(ctx context.Context, title string, year int, mediaType, tmdbKey string) (string, error) {
	// Step 1: If a TMDB API Key is configured, use TMDB Multi-Search to resolve the correct ID
	if tmdbKey != "" {
		id, err := c.ResolveViaTMDB(ctx, title, year, mediaType, tmdbKey)
		if err == nil && id != "" {
			return id, nil
		}
		log.Printf("[WARN] TMDB ID resolution failed for %q: %v. Falling back to Cinemeta.", title, err)
	}

	// Step 2: Cinemeta Fallback Path (Zero-friction default setup)
	metas, err := c.SearchByTitle(ctx, title, mediaType)
	if err != nil {
		return "", err
	}
	if len(metas) == 0 {
		return "", fmt.Errorf("no matches found on cinemeta")
	}

	// First pass: Find an item with the exact year match to resolve re-makes
	for _, meta := range metas {
		yearStr := fmt.Sprintf("%d", year)
		if strings.Contains(meta.ReleaseInfo, yearStr) {
			return meta.ID, nil
		}
	}

	// Second pass: Fall back to the first search match if year mismatch is minor
	return metas[0].ID, nil
}

// ResolveViaTMDB queries TMDB's multi-search index to resolve verified IMDb IDs and handle regional title translations
func (c *Client) ResolveViaTMDB(ctx context.Context, title string, year int, mediaType, apiKey string) (string, error) {
	// Step 1: Query TMDB multi-search (resolves movies, tv shows, and anime in parallel)
	searchURL := fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&query=%s&include_adult=true", apiKey, url.QueryEscape(title))
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("TMDB search returned HTTP %d", resp.StatusCode)
	}

	var searchResult struct {
		Results []struct {
			ID           int    `json:"id"`
			MediaType    string `json:"media_type"`
			ReleaseDate  string `json:"release_date,omitempty"`  // Movies
			FirstAirDate string `json:"first_air_date,omitempty"` // TV/Series
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&searchResult); err != nil {
		return "", err
	}

	if len(searchResult.Results) == 0 {
		return "", fmt.Errorf("no results found on TMDB")
	}

	// Step 2: Iterate over search results to identify the best, localized type & year match
	var matchedID int
	var matchedType string

	for _, item := range searchResult.Results {
		resolvedType := item.MediaType
		if resolvedType == "tv" {
			resolvedType = "series"
		}

		if resolvedType != "movie" && resolvedType != "series" {
			continue
		}

		dateStr := item.ReleaseDate
		if resolvedType == "series" {
			dateStr = item.FirstAirDate
		}

		itemYear := 0
		if len(dateStr) >= 4 {
			itemYear, _ = strconv.Atoi(dateStr[:4])
		}

		// Perfect match
		if resolvedType == mediaType && itemYear == year {
			matchedID = item.ID
			matchedType = resolvedType
			break
		}
	}

	// Fallback pass: Take the first relevant movie or series match if years have minor drift
	if matchedID == 0 {
		for _, item := range searchResult.Results {
			resolvedType := item.MediaType
			if resolvedType == "tv" {
				resolvedType = "series"
			}
			if resolvedType == "movie" || resolvedType == "series" {
				matchedID = item.ID
				matchedType = resolvedType
				break
			}
		}
	}

	if matchedID == 0 {
		return "", fmt.Errorf("no valid movie/tv results in TMDB search")
	}

	// Step 3: Query TMDB details to fetch the verified, live IMDb ID ("ttXXXXXXX")
	endpoint := "movie"
	if matchedType == "series" {
		endpoint = "tv"
	}

	externalURL := fmt.Sprintf("https://api.themoviedb.org/3/%s/%d/external_ids?api_key=%s", endpoint, matchedID, apiKey)
	extReq, err := http.NewRequestWithContext(ctx, "GET", externalURL, nil)
	if err != nil {
		return "", err
	}

	extResp, err := c.httpClient.Do(extReq)
	if err != nil {
		return "", err
	}
	defer extResp.Body.Close()

	if extResp.StatusCode != 200 {
		return "", fmt.Errorf("TMDB external IDs returned HTTP %d", extResp.StatusCode)
	}

	var extResult struct {
		IMDbID string `json:"imdb_id"`
	}

	if err := json.NewDecoder(extResp.Body).Decode(&extResult); err != nil {
		return "", err
	}

	if extResult.IMDbID == "" {
		return "", fmt.Errorf("no IMDb ID found in TMDB metadata")
	}

	return extResult.IMDbID, nil
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
