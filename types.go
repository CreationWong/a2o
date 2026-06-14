package main

import "encoding/json"

type Config struct {
	DebugLevel        string          `json:"debug_level"`
	RoundRobinAddress string          `json:"round_robin_address"`
	AuthToken         string          `json:"auth_token"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	Services          []ServiceConfig `json:"services"`
}

type ServiceConfig struct {
	Comment       string `json:"comment,omitempty"`
	ListenAddress string `json:"listen_address"`
	OpenAIBaseURL string `json:"openai_base_url"`
	OpenAIAPIKey  string `json:"openai_api_key"`
	ForceModel    string `json:"force_model,omitempty"`
	UpstreamProxy string `json:"upstream_proxy,omitempty"`
}

type ModelMapping struct {
	Upstream   string
	Downstream string
}

// AnthropicRequest --- Anthropic Structures ---
type AnthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []AnthropicMessage `json:"messages"`
	System        interface{}        `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    interface{}        `json:"tool_choice,omitempty"`
	Metadata      *AnthropicMetadata `json:"metadata,omitempty"`
}

type AnthropicMetadata struct {
	UserId string `json:"user_id,omitempty"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type AnthropicContent struct {
	Type     string                 `json:"type"`
	Text     string                 `json:"text,omitempty"`
	Thinking string                 `json:"thinking,omitempty"`
	Source   *AnthropicSource       `json:"source,omitempty"`
	Id       string                 `json:"id,omitempty"`
	Name     string                 `json:"name,omitempty"`
	Input    map[string]interface{} `json:"input,omitempty"`
}

type AnthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type AnthropicResponse struct {
	Id           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Content      []AnthropicContent `json:"content"`
	Model        string             `json:"model"`
	StopReason   *string            `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        AnthropicUsage     `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// OpenAIRequest --- OpenAI Structures ---
type OpenAIRequest struct {
	Model             string          `json:"model"`
	Messages          []OpenAIMessage `json:"messages"`
	MaxTokens         int             `json:"max_tokens,omitempty"`
	Stop              []string        `json:"stop,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Tools             []OpenAITool    `json:"tools,omitempty"`
	ToolChoice        interface{}     `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	User              string          `json:"user,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAITool struct {
	Type     string              `json:"type"`
	Function OpenAIUtilsFunction `json:"function"`
}

type OpenAIUtilsFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content,omitempty"` // string, []OpenAIContentPart, or null for tool calls only
	Refusal          string           `json:"refusal,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallId       string           `json:"tool_call_id,omitempty"`
}

type OpenAIToolCall struct {
	Index    int                `json:"index,omitempty"`
	Id       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}

type OpenAIImageURL struct {
	URL string `json:"url"`
}

type OpenAIResponse struct {
	Id      string         `json:"id"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIUsage struct {
	PromptTokens          int                    `json:"prompt_tokens"`
	CompletionTokens      int                    `json:"completion_tokens"`
	TotalTokens           int                    `json:"total_tokens"`
	PromptTokensDetails   map[string]interface{} `json:"prompt_tokens_details,omitempty"`   // vLLM: {"cached_tokens": N}
	PromptCacheHitTokens  int                    `json:"prompt_cache_hit_tokens,omitempty"` // DeepSeek native
	PromptCacheMissTokens int                    `json:"prompt_cache_miss_tokens,omitempty"`
	CacheReadInputTokens  int                    `json:"cache_read_input_tokens,omitempty"` // disk cache
}

type OpenAIChoice struct {
	Message      OpenAIMessage `json:"message"`
	FinishReason *string       `json:"finish_reason,omitempty"`
}

type OpenAIStreamResponse struct {
	Id      string               `json:"id"`
	Choices []OpenAIStreamChoice `json:"choices"`
	Usage   *OpenAIUsage         `json:"usage,omitempty"`
}

type OpenAIStreamChoice struct {
	Delta        OpenAIMessage `json:"delta"`
	FinishReason *string       `json:"finish_reason"`
}
