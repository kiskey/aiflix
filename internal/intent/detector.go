package intent

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/stremio-ai-search/internal/models"
)

var (
	// Patterns for intent detection. Replaced boundary spacing with robust digit boundaries.
	yearRegex        = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	exactTitleRegex  = regexp.MustCompile(`^(the |a |an )?[\w\s'-]+(\s+\(\d{4}\))?$`)
	yearPatternRegex = regexp.MustCompile(`\b(in|from|of|since|released|listed|around|year|em|de|desde|dans|depuis|seit)\s+\d{4}\b`)

	// Unified semantic indicators optimized with multilingual triggers & broad categorical list nouns
	semanticIndicators = []string{
		"movies about", "films about", "movies where", "films where",
		"movies with", "films with", "movies like", "films like",
		"series about", "shows about", "show about", "tv about",
		"series where", "shows where", "show where", "tv where",
		"series with", "shows with", "show with", "tv with",
		"series like", "shows like", "show like", "tv like",
		"best", "top rated", "highly rated", "recommended",
		"similar to", "in the style of", "inspired by",
		"featuring", "starring", "directed by",
		"what are some", "can you recommend", "suggest", "recommendation",
		"recommend", "similar", "like", "about", "theme",
		"top", "blockbuster", "blockbusters", "popular", "released", "?", // Broad list-style triggers
		"listed", "year", "years", "in 19", "in 20", "of 19", "of 20", "since 19", "since 20", // Year and collection triggers
		"peliculas", "filmes", "similares", "mejores", "melhores", "como", "meilleurs", "recommandes", "recommande", "similaires", "comme", "serien", "ähnliche", "beste", // Multilingual qualifiers
		"director", "directors", "actor", "actors", "cast", "movies", "films", "shows", // Broad categorical triggers
	}

	genreKeywords = []string{
		"action", "adventure", "animation", "biography", "comedy", "crime",
		"documentary", "drama", "family", "fantasy", "history", "horror",
		"musical", "mystery", "romance", "sci-fi", "science fiction",
		"sport", "thriller", "war", "western",
		"accion", "animacion", "comedia", "documental", "fantasia", "terror", "suspenso", // Localized genres
	}

	seriesIndicators = []string{
		"series", "tv show", "tv series", "television", "season",
		"episodes", "miniseries", "anime series", "cartoon series",
		"serie", "série", "séries", "episodios", "temporada", "saisons", "episodes", // Localized series indicators
	}

	movieIndicators = []string{
		"movie", "film", "cinema", "flick", "motion picture",
		"pelicula", "peliculas", "filme", "filmes", // Localized movie indicators
	}
)

// Detect analyzes a raw query and returns structured intent information using a high-performance scoring algorithm
func Detect(rawQuery string) models.SearchQuery {
	clean := strings.TrimSpace(strings.ToLower(rawQuery))
	words := strings.Fields(clean)
	wordCount := len(words)

	// 1. Extract year hint
	yearHint := 0
	if matches := yearRegex.FindAllString(clean, -1); len(matches) > 0 {
		yearHint, _ = strconv.Atoi(matches[0])
	}

	// 2. Detect media type
	mediaType := "any"
	hasSeries := containsAny(clean, seriesIndicators)
	hasMovie := containsAny(clean, movieIndicators)

	if hasSeries && !hasMovie {
		mediaType = "series"
	} else if hasMovie && !hasSeries {
		mediaType = "movie"
	}

	// 3. Compute Syntactic Semantic Score (S_intent)
	semanticScore := 0.0

	// Feature 1: Word Count Density Analysis
	if wordCount >= 5 {
		semanticScore += 1.5
	} else if wordCount <= 2 {
		semanticScore -= 1.0
	}

	// Feature 2: Unified Semantic Indicators Matching (Multilingual support integrated)
	for _, indicator := range semanticIndicators {
		if strings.Contains(clean, indicator) {
			semanticScore += 1.0
		}
	}

	// Feature 3: Question and Exclamatory Punctuation Phrasing
	if strings.Contains(rawQuery, "?") || strings.Contains(rawQuery, "!") {
		semanticScore += 1.5
	}

	// Feature 4: Temporal Prepositional Relationships (e.g. "released in 2020", "from 1999")
	if yearHint > 0 {
		if yearPatternRegex.MatchString(clean) {
			semanticScore += 1.5
		}
	}

	// Feature 5: Proper Noun Density Capitalization Analysis (Navigational vs Informational)
	if rawQuery != clean && !isAllUppercase(rawQuery) {
		capitalizedWords := 0
		rawWords := strings.Fields(rawQuery)
		for _, rw := range rawWords {
			if len(rw) > 0 && rw[0] >= 'A' && rw[0] <= 'Z' {
				capitalizedWords++
			}
		}
		// If more than 50% of the query is capitalized, it indicates proper noun named-entities
		if wordCount > 0 && float64(capitalizedWords)/float64(wordCount) >= 0.5 {
			semanticScore -= 1.2
		}
	}

	// Threshold-based classification
	var intent models.QueryIntent
	cleanTitle := clean
	if semanticScore >= 1.0 {
		intent = classifySemanticIntent(clean)
	} else {
		intent = models.IntentExactTitle
		// Clean and strip redundant trailing qualifiers to ensure Cinemeta matches perfectly
		cleanTitle = cleanExactTitle(clean)
	}

	return models.SearchQuery{
		Raw:        rawQuery,
		Clean:      clean,
		Intent:     intent,
		MediaType:  mediaType,
		YearHint:   yearHint,
		CleanTitle: cleanTitle,
	}
}

func classifySemanticIntent(clean string) models.QueryIntent {
	// Genre queries: contains genre keywords + quality descriptors
	if containsAny(clean, genreKeywords) && containsAny(clean, []string{"best", "top", "good", "great", "excellent", "mejores", "melhores", "meilleurs"}) {
		return models.IntentGenre
	}

	// Actor queries
	if containsAny(clean, []string{"starring", "with", "featuring", "actor", "actress", "cast", "con", "com", "avec"}) {
		return models.IntentActor
	}

	// Director queries
	if containsAny(clean, []string{"directed by", "by director", "films by", "movies by", "director", "dirigida por", "realise par"}) {
		return models.IntentDirector
	}

	// Similarity queries
	if containsAny(clean, []string{"like", "similar", "related", "equivalent", "resemble", "style", "inspired", "como", "comme", "semblable"}) {
		return models.IntentSimilar
	}

	return models.IntentSemantic
}

func cleanExactTitle(clean string) string {
	qualifiers := []string{
		"tv show", "tv series", "series", "show", "shows",
		"movie", "film", "films", "cinema", "flick", "flicks",
		"pelicula", "peliculas", "filme", "filmes", "serie", "série", "séries", // Localized suffixes
	}

	words := strings.Fields(clean)
	if len(words) <= 1 {
		return clean
	}

	// Check if the last word is a redundant qualifier suffix
	lastWord := words[len(words)-1]
	isQualifier := false
	for _, q := range qualifiers {
		if lastWord == q {
			isQualifier = true
			break
		}
	}

	if isQualifier {
		return strings.TrimSpace(strings.Join(words[:len(words)-1], " "))
	}

	// Also check for two-word qualifiers like "tv show" at the end of the query
	if len(words) >= 3 {
		lastTwo := words[len(words)-2] + " " + words[len(words)-1]
		for _, q := range qualifiers {
			if lastTwo == q {
				return strings.TrimSpace(strings.Join(words[:len(words)-2], " "))
			}
		}
	}

	return clean
}

func isAllUppercase(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
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
