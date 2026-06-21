package models

// MediaType represents the type of media content
type MediaType string

const (
	MediaTypeMovie  MediaType = "movie"
	MediaTypeSeries MediaType = "series"
)

// MetaPreviewItem represents a single item in Stremio catalog response
type MetaPreviewItem struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	Poster       string   `json:"poster,omitempty"`
	Genres       []string `json:"genres,omitempty"`
	IMDbRating   string   `json:"imdbRating,omitempty"`
	ReleaseInfo  string   `json:"releaseInfo,omitempty"`
	Description  string   `json:"description,omitempty"`
	// Series-specific fields
	EpisodeCount *int     `json:"episodeCount,omitempty"`
	SeasonCount  *int     `json:"seasonCount,omitempty"`
	Status       string   `json:"status,omitempty"`
}

// CatalogResponse is the Stremio catalog endpoint response format
type CatalogResponse struct {
	Metas []MetaPreviewItem `json:"metas"`
}

// Manifest is the addon descriptor served at /manifest.json
type Manifest struct {
	ID            string         `json:"id"`
	Version       string         `json:"version"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Types         []string       `json:"types"`
	IDPrefixes    []string       `json:"idPrefixes"`
	Resources     []ResourceItem `json:"resources"`
	Catalogs      []CatalogItem  `json:"catalogs"`
	BehaviorHints BehaviorHints  `json:"behaviorHints"`
}

type ResourceItem struct {
	Name       string   `json:"name"`
	Types      []string `json:"types,omitempty"`
	IDPrefixes []string `json:"idPrefixes,omitempty"`
}

type CatalogItem struct {
	Type           string      `json:"type"`
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Extra          []ExtraItem `json:"extra,omitempty"`
	ExtraSupported []string    `json:"extraSupported,omitempty"`
}

type ExtraItem struct {
	Name       string   `json:"name"`
	IsRequired bool     `json:"isRequired,omitempty"`
	Options    []string `json:"options,omitempty"`
}

type BehaviorHints struct {
	Configurable          bool `json:"configurable"`
	ConfigurationRequired bool `json:"configurationRequired"`
}

// MetaResponse is the full metadata response from Cinemeta
type MetaResponse struct {
	Meta MetaPreviewItem `json:"meta"`
}
