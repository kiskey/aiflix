package intent

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/stremio-ai-search/internal/models"
)

var (
	// Patterns for intent detection. Replaced boundary spacing with robust digit boundaries.
	yearRegex       = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	exactTitleRegex = regexp.MustCompile(`^(the |a |an )?[\w\s'-]+(\s+\(\d{4}\))?$`)

	// Natural language indicators
	semanticIndicators = []string{
		"movies about", "films about", "movies where", "films where",
		"movies with", "films with", "movies like", "films like",
		"best", "top rated", "highly rated", "recommended",
		"similar to", "in the style of", "inspired by",
		"featuring", "starring", "directed by",
		"what are some", "can you recommend", "suggest",
	}

	genreKeywords = []string{
		"action", "adventure", "animation", "biography", "comedy", "crime",
		"documentary", "drama", "family", "fantasy", "history", "horror",
		"musical", "mystery", "romance", "sci-fi", "science fiction",
		"sport", "thriller", "war", "western",
	}

	seriesIndicators = []string{
		"series", "tv show", "tv series", "television", "season",
		"episodes", "miniseries", "anime series", "cartoon series",
	}

	movieIndicators = []string{
		"movie", "film", "cinema", "flick", "motion picture",
	}
)

// Detect analyzes a raw query and returns structured intent information
func Detect(rawQuery string) models.SearchQuery {
	clean := strings.TrimSpace(strings.ToLower(rawQuery))

	// Extract year hint safely using isolated boundaries
	yearHint := 0
	if matches := yearRegex.FindAllString(clean, -1); len(matches) > 0 {
		yearHint, _ = strconv.Atoi(matches[0])
	}

	// Detect media type
	mediaType := "any"
	hasSeries := containsAny(clean, seriesIndicators)
	hasMovie := containsAny(clean, movieIndicators)

	if hasSeries && !hasMovie {
		mediaType = "series"
	} else if hasMovie && !hasSeries {
		mediaType = "movie"
	}

	// Detect intent
	intent := detectIntent(clean)

	return models.SearchQuery{
		Raw:       rawQuery,
		Clean:     clean,
		Intent:    intent,
		MediaType: mediaType,
		YearHint:  yearHint,
	}
}

func detectIntent(clean string) models.QueryIntent {
	// Check for exact title match first
	if isExactTitle(clean) {
		return models.IntentExactTitle
	}

	// Check for genre-based queries
	if containsAny(clean, genreKeywords) && containsAny(clean, []string{"best", "top", "good", "great"}) {
		return models.IntentGenre
	}

	// Check for actor queries
	if containsAny(clean, []string{"starring", "with", "featuring", "actor", "actress"}) {
		return models.IntentActor
	}

	// Check for director queries
	if containsAny(clean, []string{"directed by", "by director", "films by", "movies by"}) {
		return models.IntentDirector
	}

	// Check for similarity queries
	if containsAny(clean, []string{"like", "similar to", "in the style of", "inspired by"}) {
		return models.IntentSimilar
	}

	// Default to semantic search
	return models.IntentSemantic
}

func isExactTitle(query string) bool {
	// Simple heuristic: short query without semantic indicators
	if len(query) > 60 {
		return false
	}

	for _, indicator := range semanticIndicators {
		if strings.Contains(query, indicator) {
			return false
		}
	}

	return true
}

func containsAny(s string, substrs []string) bool {
	for _, substr := range substrs {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}
