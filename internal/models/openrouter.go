package models

// Message represents a single message in the conversation
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ResponseFormat controls structured output from the model
type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

// JSONSchema defines the expected output structure.
// Fields realigned: Strict (bool, 1-byte) moved to the end to eliminate memory padding gaps.
type JSONSchema struct {
	Name   string                 `json:"name"`
	Schema map[string]interface{} `json:"schema"`
	Strict bool                   `json:"strict"`
}

// Usage contains token usage statistics
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// APIError represents an error from OpenRouter.
// Fields realigned: String values are grouped together followed by integers.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    int    `json:"code,omitempty"`
}
