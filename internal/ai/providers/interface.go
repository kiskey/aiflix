package providers

import (
	"context"
	"strings"

	"github.com/stremio-ai-search/internal/models"
)

// AIProvider is the interface that all AI inference providers must implement
type AIProvider interface {
	Name() string
	ChatCompletion(ctx context.Context, req models.UnifiedChatRequest) (*models.UnifiedChatResponse, error)
	GetModels() []string
	GetMaxRPM() int
	GetMaxRPD() int
}

// cleanJSONContent strips potential markdown block indicators around JSON output
func cleanJSONContent(content string) string {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	return content
}
