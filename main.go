package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- Configuration Structs ---

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

var config Config
var usageLogChan = make(chan UsageRecord, 5000)

type UsageRecord struct {
	Time       time.Time
	Service    string
	Model      string
	DurationMs int64
	Prompt     int
	Completion int
	Total      int
}

// --- Runtime State ---
var rrCounter atomic.Uint64

// --- Connection Pool ---
var (
	clientPool   = make(map[string]*http.Client)
	clientPoolMu sync.RWMutex
)

// --- Object Pools ---
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func getOrCreateClient(svc ServiceConfig) *http.Client {
	key := svc.OpenAIBaseURL + "|" + svc.UpstreamProxy

	clientPoolMu.RLock()
	if client, ok := clientPool[key]; ok {
		clientPoolMu.RUnlock()
		return client
	}
	clientPoolMu.RUnlock()

	clientPoolMu.Lock()
	defer clientPoolMu.Unlock()
	if client, ok := clientPool[key]; ok {
		return client
	}

	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Duration(config.TimeoutSeconds) * time.Second,
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}

	if svc.UpstreamProxy != "" {
		if proxyUrl, err := url.Parse(svc.UpstreamProxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyUrl)
		} else {
			log.Printf("[WARN] Invalid proxy URL for %s: %v", svc.OpenAIBaseURL, err)
		}
	}

	client := &http.Client{Transport: transport}
	clientPool[key] = client
	log.Printf("[POOL] Created new HTTP client for %s", key)
	return client
}

// --- Anthropic Structures ---
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

// --- OpenAI Structures ---
type OpenAIRequest struct {
	Model     string          `json:"model"`
	Messages  []OpenAIMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Tools               []OpenAITool    `json:"tools,omitempty"`
	ToolChoice          interface{}     `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	User                string          `json:"user,omitempty"`
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
	PromptTokensDetails   map[string]interface{} `json:"prompt_tokens_details,omitempty"`  // vLLM: {"cached_tokens": N}
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

// --- Main ---
func main() {
	configFile := flag.String("config", "config.json", "Path to config file")
	flag.Parse()
	loadConfig(*configFile)

	fmt.Printf("A2O Proxy Config Loaded. DebugLevel: %s\n", config.DebugLevel)

	go aggregatorWorker()

	if len(config.Services) == 0 {
		log.Fatal("No services defined in config.")
	}

	var wg sync.WaitGroup
	for i, svc := range config.Services {
		wg.Add(1)
		go func(idx int, s ServiceConfig) {
			defer wg.Done()
			mux := http.NewServeMux()
			handler := makeHandler(func() ServiceConfig { return s }, s.ListenAddress)
			mux.HandleFunc("/v1/messages", handler)
			modelsHandler := makeModelsHandler(func() ServiceConfig { return s })
			mux.HandleFunc("/v1/models", modelsHandler)
			mux.HandleFunc("/models", modelsHandler)
			addCommonEndpoints(mux)
			log.Printf("Starting Service #%d on %s (%s)", idx+1, s.ListenAddress, s.Comment)
			if err := http.ListenAndServe(fixAddr(s.ListenAddress), mux); err != nil {
				log.Printf("[ERR] Service %s failed: %v", s.ListenAddress, err)
			}
		}(i, svc)
	}

	if config.RoundRobinAddress != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mux := http.NewServeMux()
			rrProvider := func() ServiceConfig {
				count := rrCounter.Add(1)
				idx := (count - 1) % uint64(len(config.Services))
				selected := config.Services[idx]
				logDebug("[RR-LB] Selected Service #%d for request", idx+1)
				return selected
			}
			handler := makeHandler(rrProvider, config.RoundRobinAddress)
			mux.HandleFunc("/v1/messages", handler)
			rrModelsHandler := makeModelsHandler(rrProvider)
			mux.HandleFunc("/v1/models", rrModelsHandler)
			mux.HandleFunc("/models", rrModelsHandler)
			addCommonEndpoints(mux)
			log.Printf("Starting Global Round-Robin Listener on %s", config.RoundRobinAddress)
			if err := http.ListenAndServe(fixAddr(config.RoundRobinAddress), mux); err != nil {
				log.Printf("[ERR] Round-Robin Listener %s failed: %v", config.RoundRobinAddress, err)
			}
		}()
	}

	wg.Wait()
}

func fixAddr(addr string) string {
	if !strings.Contains(addr, ":") {
		return ":" + addr
	}
	return addr
}

func addCommonEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Simple estimation: approximate 1 token per 4 chars
		body, _ := io.ReadAll(r.Body)
		count := len(body) / 4
		if count < 1 {
			count = 1
		}
		json.NewEncoder(w).Encode(map[string]int{"input_tokens": count})
	})
}

func makeModelsHandler(getServiceConfig configProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method Not Allowed", 405)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		svc := getServiceConfig()

		// FORCE_MODEL 指定时直接返回该模型
		if svc.ForceModel != "" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"object": "list",
				"data": []map[string]interface{}{
					{
						"id":       svc.ForceModel,
						"object":   "model",
						"owned_by": "deepseek",
					},
				},
			})
			return
		}

		// 未指定 FORCE_MODEL: 转发到上游 /v1/models
		client := getOrCreateClient(svc)
		upstreamURL := svc.OpenAIBaseURL
		if upstreamURL == "" {
			http.Error(w, "Upstream Not Configured", 502)
			return
		}
		upstreamURL = strings.Replace(upstreamURL, "/chat/completions", "/models", 1)

		req, _ := http.NewRequestWithContext(r.Context(), "GET", upstreamURL, nil)
		if svc.OpenAIAPIKey != "" {
			req.Header.Set("Authorization", "Bearer "+svc.OpenAIAPIKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[ERR] Models upstream request failed: %v", err)
			http.Error(w, "Upstream Error", 502)
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
	}
}

func loadConfig(path string) {
	// 从环境变量读取所有配置
	config = Config{
		DebugLevel:        getEnv("DEBUG_LEVEL", "info"),
		AuthToken:         os.Getenv("AUTH_TOKEN"),
		RoundRobinAddress: os.Getenv("ROUND_ROBIN_ADDRESS"),
		TimeoutSeconds:    getEnvInt("TIMEOUT_SECONDS", 300),
		Services: []ServiceConfig{
			{
				Comment:       getEnv("SERVICE_COMMENT", "default"),
				ListenAddress: getEnv("LISTEN_ADDRESS", "9999"),
				OpenAIBaseURL: os.Getenv("OPENAI_BASE_URL"),
				OpenAIAPIKey:  os.Getenv("OPENAI_API_KEY"),
				ForceModel:    os.Getenv("FORCE_MODEL"),
				UpstreamProxy: os.Getenv("UPSTREAM_PROXY"),
			},
		},
	}

	// 配置文件存在则合并（env 优先）
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		var fileCfg Config
		if err := json.NewDecoder(file).Decode(&fileCfg); err != nil {
			log.Fatalf("[FATAL] Failed to parse config: %v", err)
		}
		// 仅当 env 未设置时，用配置文件的值
		if os.Getenv("DEBUG_LEVEL") == "" && fileCfg.DebugLevel != "" {
			config.DebugLevel = fileCfg.DebugLevel
		}
		if os.Getenv("AUTH_TOKEN") == "" && fileCfg.AuthToken != "" {
			config.AuthToken = fileCfg.AuthToken
		}
		if os.Getenv("ROUND_ROBIN_ADDRESS") == "" && fileCfg.RoundRobinAddress != "" {
			config.RoundRobinAddress = fileCfg.RoundRobinAddress
		}
		if os.Getenv("TIMEOUT_SECONDS") == "" && fileCfg.TimeoutSeconds != 0 {
			config.TimeoutSeconds = fileCfg.TimeoutSeconds
		}
		if os.Getenv("OPENAI_BASE_URL") == "" && len(fileCfg.Services) > 0 && fileCfg.Services[0].OpenAIBaseURL != "" {
			config.Services[0].OpenAIBaseURL = fileCfg.Services[0].OpenAIBaseURL
		}
		if os.Getenv("OPENAI_API_KEY") == "" && len(fileCfg.Services) > 0 && fileCfg.Services[0].OpenAIAPIKey != "" {
			config.Services[0].OpenAIAPIKey = fileCfg.Services[0].OpenAIAPIKey
		}
		if os.Getenv("FORCE_MODEL") == "" && len(fileCfg.Services) > 0 && fileCfg.Services[0].ForceModel != "" {
			config.Services[0].ForceModel = fileCfg.Services[0].ForceModel
		}
		if os.Getenv("UPSTREAM_PROXY") == "" && len(fileCfg.Services) > 0 && fileCfg.Services[0].UpstreamProxy != "" {
			config.Services[0].UpstreamProxy = fileCfg.Services[0].UpstreamProxy
		}
		if os.Getenv("LISTEN_ADDRESS") == "" && len(fileCfg.Services) > 0 && fileCfg.Services[0].ListenAddress != "" {
			config.Services[0].ListenAddress = fileCfg.Services[0].ListenAddress
		}
		if os.Getenv("SERVICE_COMMENT") == "" && len(fileCfg.Services) > 0 && fileCfg.Services[0].Comment != "" {
			config.Services[0].Comment = fileCfg.Services[0].Comment
		}
		// 多服务场景：如果配置文件定义了多个 service，全部追加
		if len(fileCfg.Services) > 1 {
			config.Services = append(config.Services, fileCfg.Services[1:]...)
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &fallback); err == nil && n == 1 {
			return fallback
		}
	}
	return fallback
}

func logDebug(format string, v ...interface{}) {
	if config.DebugLevel == "debug" {
		log.Printf(format, v...)
	}
}

func logCacheInfo(listenAddr string, headers http.Header, usage *OpenAIUsage) {
	if config.DebugLevel != "debug" {
		return
	}
	cacheHdr := headers.Get("X-DS-Cache-Hit")
	if cacheHdr == "" {
		cacheHdr = headers.Get("X-Cache")
	}
	if cacheHdr == "" {
		cacheHdr = headers.Get("X-Cache-Status")
	}
	if cacheHdr == "" {
		cacheHdr = headers.Get("CF-Cache-Status")
	}
	if cacheHdr != "" {
		log.Printf("[%s] 🔗 Cache Header: %s", listenAddr, cacheHdr)
	}
	if usage != nil {
		if usage.PromptCacheHitTokens > 0 {
			log.Printf("[%s] 🔗 Cache Hit: %d tokens (miss: %d)", listenAddr, usage.PromptCacheHitTokens, usage.PromptCacheMissTokens)
		}
		if usage.CacheReadInputTokens > 0 {
			log.Printf("[%s] 🔗 Disk Cache Hit: %d tokens", listenAddr, usage.CacheReadInputTokens)
		}
	}
}

func logCacheUsage(model string, usage *OpenAIUsage) {
	if usage == nil {
		return
	}
	// vLLM format: prompt_tokens_details.cached_tokens
	if details := usage.PromptTokensDetails; details != nil {
		if cached, ok := details["cached_tokens"].(float64); ok && cached > 0 {
			log.Printf("[%s] 💾 Prefix Cache Hit: %.0f cached tokens (vLLM)", model, cached)
			return
		}
	}
	// DeepSeek native format
	if usage.PromptCacheHitTokens > 0 {
		log.Printf("[%s] 💾 Prefix Cache Hit: %d tokens, miss: %d (DeepSeek native)",
			model, usage.PromptCacheHitTokens, usage.PromptCacheMissTokens)
	}
	if usage.CacheReadInputTokens > 0 {
		log.Printf("[%s] 💾 Disk Cache Hit: %d tokens", model, usage.CacheReadInputTokens)
	}
}

// unwrapToolInput returns a new map with any nested "arguments" wrapper removed.
// It does not modify the input map.
func unwrapToolInput(input map[string]interface{}) map[string]interface{} {
	// start with a shallow copy
	current := make(map[string]interface{})
	for k, v := range input {
		current[k] = v
	}
	for {
		nested, exists := current["arguments"]
		if !exists {
			break
		}
		switch v := nested.(type) {
		case map[string]interface{}:
			if len(current) == 1 {
				// whole map is just arguments -> unwrap one level
				newMap := make(map[string]interface{})
				for k, val := range v {
					newMap[k] = val
				}
				current = newMap
				continue
			} else {
				// merge arguments into parent and stop
				delete(current, "arguments")
				for k, val := range v {
					current[k] = val
				}
				break
			}
		case string:
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(v), &parsed); err == nil {
				if len(current) == 1 {
					current = parsed
					continue
				} else {
					delete(current, "arguments")
					for k, val := range parsed {
						current[k] = val
					}
					break
				}
			}
		}
		break
	}
	return current
}

type configProvider func() ServiceConfig

func makeHandler(getServiceConfig configProvider, listenAddr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		log.Printf("[%s] >>> Request: %s %s from %s", listenAddr, r.Method, r.URL.Path, r.RemoteAddr)
		logDebug("[%s] Headers: %v", listenAddr, r.Header)

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "Method Not Allowed", 405)
			return
		}

		if config.AuthToken != "" {
			clientKey := r.Header.Get("x-api-key")
			if clientKey == "" {
				clientKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if clientKey != config.AuthToken {
				log.Printf("[AUTH] Failed auth attempt from %s", r.RemoteAddr)
				http.Error(w, "Unauthorized: Invalid API Key", 401)
				return
			}
		}

		svc := getServiceConfig()
		svcName := svc.Comment
		if svcName == "" {
			svcName = svc.ListenAddress
		}

		var antReq AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&antReq); err != nil {
			logDebug("[%s] Bad request body: %v", listenAddr, err)
			http.Error(w, "Bad Request", 400)
			return
		}

		targetModel := antReq.Model
		if svc.ForceModel != "" {
			targetModel = svc.ForceModel
		}

		logDebug("[%s] REQ Model: %s -> %s, Stream: %v", listenAddr, antReq.Model, targetModel, antReq.Stream)

		oaiReq, err := convertToOpenAI(&antReq, targetModel)
		if err != nil {
			log.Printf("[ERR] Convert failed: %v", err)
			http.Error(w, "Convert Error", 400)
			return
		}

		buf := bufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		json.NewEncoder(buf).Encode(oaiReq)
		oaiBody := buf.Bytes()
		defer bufferPool.Put(buf)

		authKey := svc.OpenAIAPIKey
		if authKey == "" {
			k := r.Header.Get("x-api-key")
			if k == "" {
				k = r.Header.Get("Authorization")
			}
			authKey = strings.TrimPrefix(k, "Bearer ")
		}

		client := getOrCreateClient(svc)

		var resp *http.Response
		var upstreamErr error
		var finalBody io.Reader
		maxRetries := 3

		for i := 0; i < maxRetries; i++ {
			logDebug("[%s] Sending upstream request (attempt %d/%d)", listenAddr, i+1, maxRetries)

			req, _ := http.NewRequestWithContext(r.Context(), "POST", svc.OpenAIBaseURL, bytes.NewBuffer(oaiBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+authKey)

			resp, upstreamErr = client.Do(req)
			if upstreamErr != nil {
				log.Printf("[WARN] Upstream attempt %d/%d failed: %v", i+1, maxRetries, upstreamErr)
				goto RETRY_WAIT
			}

			if resp.StatusCode != 200 {
				finalBody = resp.Body
				break
			}

			if antReq.Stream {
				peekCtx, peekCancel := context.WithCancel(r.Context())
				var peekBuf bytes.Buffer
				peekReader := bufio.NewReader(resp.Body)
				success := false

				done := make(chan bool)
				go func() {
					defer close(done)
					for {
						select {
						case <-peekCtx.Done():
							return
						default:
						}
						line, err := peekReader.ReadBytes('\n')
						if len(line) > 0 {
							peekBuf.Write(line)
						}
						if err != nil {
							return
						}
						if strings.HasPrefix(string(line), "data:") {
							success = true
							return
						}
					}
				}()

				select {
				case <-done:
				case <-time.After(5 * time.Second):
					peekCancel()
					log.Printf("[WARN] Stream Peek Timeout after 5s")
					success = false
				}
				peekCancel()

				if success {
					finalBody = io.MultiReader(&peekBuf, peekReader)
					break
				} else {
					resp.Body.Close()
					upstreamErr = fmt.Errorf("stream peek failed")
				}
			} else {
				finalBody = resp.Body
				break
			}

		RETRY_WAIT:
			if i < maxRetries-1 {
				select {
				case <-r.Context().Done():
					http.Error(w, "Client Disconnected", 499)
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
		}

		if upstreamErr != nil {
			if resp != nil {
				resp.Body.Close()
			}
			log.Printf("[ERR] Upstream Call Failed after %d retries: %v", maxRetries, upstreamErr)
			http.Error(w, "Upstream Error", 502)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(finalBody)
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			log.Printf("[ERR] Upstream replied %s: %s", resp.Status, string(body))
			return
		}

			logCacheInfo(listenAddr, resp.Header, nil)
		if antReq.Stream {
			handleStream(w, finalBody, antReq.Model, svcName, start)
		} else {
			handleNormal(w, finalBody, antReq.Model, svcName, start)
		}
	}
}

// convertToOpenAI now preserves content block order by splitting assistant messages
// with interleaved text/tool_use into multiple messages, and user messages with
// interleaved tool_results into proper ordered sequences.
func convertToOpenAI(ant *AnthropicRequest, targetModel string) (*OpenAIRequest, error) {
	oai := &OpenAIRequest{
		Model:               targetModel,
		MaxTokens:         ant.MaxTokens,
		Stream:              ant.Stream,
		Temperature:         ant.Temperature,
		TopP:                ant.TopP,
		Stop:                ant.StopSequences,
		Messages:            []OpenAIMessage{},
	}
	if ant.Stream {
		oai.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// tool choice conversion
	if ant.ToolChoice != nil {
		switch tc := ant.ToolChoice.(type) {
		case string:
			switch tc {
			case "auto":
				oai.ToolChoice = "auto"
			case "any", "required":
				oai.ToolChoice = "required"
			case "none":
				oai.ToolChoice = "none"
			}
		case map[string]interface{}:
			tcType, _ := tc["type"].(string)
			switch tcType {
			case "auto":
				oai.ToolChoice = "auto"
			case "any", "required":
				oai.ToolChoice = "required"
			case "none":
				oai.ToolChoice = "none"
			case "tool":
				if toolName, ok := tc["name"].(string); ok {
					oai.ToolChoice = map[string]interface{}{
						"type": "function",
						"function": map[string]string{
							"name": toolName,
						},
					}
				}
			}
			if disableParallel, ok := tc["disable_parallel_tool_use"].(bool); ok && disableParallel {
				parallelFalse := false
				oai.ParallelToolCalls = &parallelFalse
			}
		}
	}

	if ant.Metadata != nil && ant.Metadata.UserId != "" {
		oai.User = ant.Metadata.UserId
	}

	if len(ant.Tools) > 0 {
		oai.Tools = make([]OpenAITool, len(ant.Tools))
		for i, t := range ant.Tools {
			oai.Tools[i] = OpenAITool{
				Type: "function",
				Function: OpenAIUtilsFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			}
		}
	}

	// system prompt
	var systemPrompt string
	if ant.System != nil {
		if s, ok := ant.System.(string); ok {
			systemPrompt = s
		} else if arr, ok := ant.System.([]interface{}); ok {
			var sb strings.Builder
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if m["type"] == "text" {
						if txt, ok := m["text"].(string); ok {
							sb.WriteString(txt)
							sb.WriteByte('\n')
						}
					}
				}
			}
			systemPrompt = sb.String()
		}
	}
	if systemPrompt != "" {
		content, _ := json.Marshal(systemPrompt)
		oai.Messages = append(oai.Messages, OpenAIMessage{
			Role:    "system",
			Content: content,
		})
	}

	// Convert each Anthropic message preserving block order
	for _, msg := range ant.Messages {
		if msg.Role == "assistant" {
			blocks := extractContentBlocks(msg.Content)
			if len(blocks) == 0 {
				continue
			}
			// Group consecutive text and thinking blocks, and separate tool_use blocks
			// We'll create one assistant message per contiguous text+thinking group,
			// with optional tool_calls from preceding tool_use blocks.
			// This ensures order: if original is [text, tool_use, text], we produce:
			// assistant with text, then assistant with tool_calls (no content), then assistant with text.
			oai.Messages = append(oai.Messages, buildOrderedAssistantMessages(blocks)...)
		} else if msg.Role == "user" {
			blocks := extractContentBlocks(msg.Content)
			if len(blocks) == 0 {
				continue
			}
			oai.Messages = append(oai.Messages, buildOrderedUserMessages(blocks)...)
		}
	}

	return oai, nil
}

// contentBlock represents a parsed block from an Anthropic message content array.
type contentBlock struct {
	Type       string                 // "text", "thinking", "tool_use", "tool_result", "image"
	Text       string                 // for text/thinking
	Source     map[string]interface{} // for image
	Id         string                 // for tool_use/tool_result
	Name       string                 // for tool_use
	Input      map[string]interface{} // for tool_use
	ToolUseId  string                 // for tool_result
	IsError    bool                   // for tool_result
	Content    interface{}            // raw tool_result content
}

func extractContentBlocks(content interface{}) []contentBlock {
	if content == nil {
		return nil
	}
	switch c := content.(type) {
	case string:
		return []contentBlock{{Type: "text", Text: c}}
	case []interface{}:
		var blocks []contentBlock
		for _, item := range c {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			bType, _ := m["type"].(string)
			switch bType {
			case "text":
				txt, _ := m["text"].(string)
				blocks = append(blocks, contentBlock{Type: "text", Text: txt})
			case "thinking":
				think, _ := m["thinking"].(string)
				blocks = append(blocks, contentBlock{Type: "thinking", Text: think})
			case "tool_use":
				id, _ := m["id"].(string)
				name, _ := m["name"].(string)
				input, _ := m["input"].(map[string]interface{})
				if input == nil {
					input = make(map[string]interface{})
				}
				blocks = append(blocks, contentBlock{
					Type:  "tool_use",
					Id:    id,
					Name:  name,
					Input: input,
				})
			case "tool_result":
				id, _ := m["tool_use_id"].(string)
				isError, _ := m["is_error"].(bool)
				blocks = append(blocks, contentBlock{
					Type:      "tool_result",
					ToolUseId: id,
					IsError:   isError,
					Content:   m["content"],
				})
			case "image":
				src, _ := m["source"].(map[string]interface{})
				blocks = append(blocks, contentBlock{
					Type:   "image",
					Source: src,
				})
			}
		}
		return blocks
	default:
		// fallback: marshal and try to parse
		b, _ := json.Marshal(content)
		var list []map[string]interface{}
		if json.Unmarshal(b, &list) == nil {
			return extractContentBlocks(list)
		}
		return nil
	}
}

// buildOrderedAssistantMessages creates a sequence of OpenAI assistant messages
// from the ordered list of content blocks, preserving interleaving.
func buildOrderedAssistantMessages(blocks []contentBlock) []OpenAIMessage {
	var msgs []OpenAIMessage
	var textBuf strings.Builder
	var thinkingBuf strings.Builder
	var toolCalls []OpenAIToolCall
	flushText := func() {
		if textBuf.Len() > 0 || thinkingBuf.Len() > 0 || len(toolCalls) > 0 {
			msg := OpenAIMessage{Role: "assistant"}
			if thinkingBuf.Len() > 0 {
				msg.ReasoningContent = thinkingBuf.String()
			}
			if textBuf.Len() > 0 {
				b, _ := json.Marshal(textBuf.String())
				msg.Content = b
			}
			if len(toolCalls) > 0 {
				msg.ToolCalls = toolCalls
				toolCalls = nil
			}
			msgs = append(msgs, msg)
			textBuf.Reset()
			thinkingBuf.Reset()
		}
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if len(toolCalls) > 0 {
				// text after tool_use: flush previous tool_use message first
				flushText()
			}
			if thinkingBuf.Len() > 0 {
				// thinking before text: flush thinking as separate message? We'll keep them together in one message.
				textBuf.WriteString(block.Text)
			} else {
				textBuf.WriteString(block.Text)
			}
		case "thinking":
			if len(toolCalls) > 0 {
				flushText()
			}
			thinkingBuf.WriteString(block.Text)
		case "tool_use":
			// flush any accumulated text/thinking before tool calls
			if textBuf.Len() > 0 || thinkingBuf.Len() > 0 {
				flushText()
			}
			cleanInput := unwrapToolInput(block.Input)
			inputJson, _ := json.Marshal(cleanInput)
			toolCalls = append(toolCalls, OpenAIToolCall{
				Id:   block.Id,
				Type: "function",
				Function: OpenAIFunctionCall{
					Name:      block.Name,
					Arguments: string(inputJson),
				},
			})
		}
	}
	flushText() // remaining
	return msgs
}

// buildOrderedUserMessages creates a sequence of OpenAI user/tool messages
// from the ordered list of content blocks.
func buildOrderedUserMessages(blocks []contentBlock) []OpenAIMessage {
	var msgs []OpenAIMessage
	var userParts []OpenAIContentPart // for text and image blocks accumulated for a single user message
	flushUser := func() {
		if len(userParts) > 0 {
			b, _ := json.Marshal(userParts)
			msgs = append(msgs, OpenAIMessage{Role: "user", Content: b})
			userParts = nil
		}
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			userParts = append(userParts, OpenAIContentPart{Type: "text", Text: block.Text})
		case "image":
			imgUrl := imageSourceToURL(block.Source)
			if imgUrl != "" {
				userParts = append(userParts, OpenAIContentPart{
					Type:     "image_url",
					ImageURL: &OpenAIImageURL{URL: imgUrl},
				})
			}
		case "tool_result":
			// flush any accumulated user parts before tool result
			flushUser()
			// convert tool_result to tool message
			toolMsg := buildToolMessage(block)
			msgs = append(msgs, toolMsg)
		}
	}
	flushUser()
	return msgs
}

func imageSourceToURL(src map[string]interface{}) string {
	if src == nil {
		return ""
	}
	srcType, _ := src["type"].(string)
	if srcType == "url" {
		url, _ := src["url"].(string)
		return url
	}
	mediaType, _ := src["media_type"].(string)
	data, _ := src["data"].(string)
	if mediaType != "" && data != "" {
		return fmt.Sprintf("data:%s;base64,%s", mediaType, data)
	}
	return ""
}

func buildToolMessage(block contentBlock) OpenAIMessage {
	var resultText string
	var imageParts []OpenAIContentPart

	switch c := block.Content.(type) {
	case string:
		resultText = c
	case []interface{}:
		for _, sub := range c {
			subMap, ok := sub.(map[string]interface{})
			if !ok {
				continue
			}
			subType, _ := subMap["type"].(string)
			if subType == "text" {
				txt, _ := subMap["text"].(string)
				resultText += txt
			} else if subType == "image" {
				src, _ := subMap["source"].(map[string]interface{})
				imgUrl := imageSourceToURL(src)
				if imgUrl != "" {
					imageParts = append(imageParts, OpenAIContentPart{
						Type:     "image_url",
						ImageURL: &OpenAIImageURL{URL: imgUrl},
					})
				}
			}
		}
	default:
		b, _ := json.Marshal(block.Content)
		resultText = string(b)
	}

	if block.IsError {
		resultText = "[ERROR] " + resultText
	}

	if len(imageParts) > 0 {
		var contentParts []OpenAIContentPart
		if resultText != "" {
			contentParts = append(contentParts, OpenAIContentPart{Type: "text", Text: resultText})
		}
		contentParts = append(contentParts, imageParts...)
		contentJson, _ := json.Marshal(contentParts)
		return OpenAIMessage{Role: "tool", ToolCallId: block.ToolUseId, Content: contentJson}
	}
	return OpenAIMessage{Role: "tool", ToolCallId: block.ToolUseId, Content: json.RawMessage(fmt.Sprintf("%q", resultText))}
}

func handleNormal(w http.ResponseWriter, body io.Reader, model string, svcName string, startTime time.Time) {
	var oaiResp OpenAIResponse
	if err := json.NewDecoder(body).Decode(&oaiResp); err != nil {
		http.Error(w, "Upstream decode error", 500)
		return
	}

	select {
	case usageLogChan <- UsageRecord{
		Time:       startTime,
		Service:    svcName,
		Model:      model,
		DurationMs: time.Since(startTime).Milliseconds(),
		Prompt:     oaiResp.Usage.PromptTokens,
		Completion: oaiResp.Usage.CompletionTokens,
		Total:      oaiResp.Usage.TotalTokens,
	}:
	default:
	}

	logCacheUsage(model, &oaiResp.Usage)
	antResp := AnthropicResponse{
		Id:      "msg_" + oaiResp.Id,
		Type:    "message",
		Role:    "assistant",
		Model:   model,
		Content: []AnthropicContent{},
		Usage: AnthropicUsage{
			InputTokens:  oaiResp.Usage.PromptTokens,
			OutputTokens: oaiResp.Usage.CompletionTokens,
		},
	}

	if len(oaiResp.Choices) > 0 {
		choice := oaiResp.Choices[0]
		msg := choice.Message

		// We need to reconstruct the original order from the OpenAI message.
		// OpenAI can return content (string) and tool_calls, plus optional reasoning_content.
		// The order in Anthropic might be: thinking -> text -> tool_use, etc.
		// We'll assume standard order: reasoning_content (thinking) first, then text, then tool_calls.
		// This is a simplification but matches common patterns.
		if msg.ReasoningContent != "" {
			antResp.Content = append(antResp.Content, AnthropicContent{
				Type:     "thinking",
				Thinking: msg.ReasoningContent,
			})
		}

		// tool calls
		for _, tc := range msg.ToolCalls {
			var args map[string]interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = make(map[string]interface{})
			}
			cleanArgs := unwrapToolInput(args)
			antResp.Content = append(antResp.Content, AnthropicContent{
				Type:  "tool_use",
				Id:    tc.Id,
				Name:  tc.Function.Name,
				Input: cleanArgs,
			})
		}

		// text content
		if len(msg.Content) > 0 {
			var s string
			json.Unmarshal(msg.Content, &s)
			if s != "" {
				antResp.Content = append(antResp.Content, AnthropicContent{Type: "text", Text: s})
			}
		}

		reason := "end_turn"
		if choice.FinishReason != nil {
			fr := *choice.FinishReason
			switch fr {
			case "length":
				reason = "max_tokens"
			case "tool_calls", "function_call":
				reason = "tool_use"
			case "content_filter":
				reason = "content_filter"
			case "stop":
				reason = "end_turn"
			}
		} else if len(msg.ToolCalls) > 0 {
			reason = "tool_use"
		}
		antResp.StopReason = &reason
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(antResp)
}

func handleStream(w http.ResponseWriter, body io.Reader, model string, svcName string, startTime time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	msgId := fmt.Sprintf("msg_%d", time.Now().Unix())
	sendEvent(w, "message_start", map[string]interface{}{
		"message": map[string]interface{}{
			"id": msgId, "type": "message", "role": "assistant", "content": []string{},
			"model": model, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	flusher.Flush()

	currentBlockIndex := -1
	currentBlockType := ""
	var finalUsage *OpenAIUsage
	finishReason := "end_turn"
	chunkCount := 0

	toolIndexMap := make(map[int]int)      // openai tool index -> anthropic block index
	toolArgBuf := make(map[int]string)     // buffered arguments per tool index
	toolFirstResolved := make(map[int]bool)
	toolNested := make(map[int]bool)
	toolSent := make(map[int]bool)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		chunkCount++
		var chunk OpenAIStreamResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			log.Printf("[WARN] Stream JSON parse error: %v", err)
			continue
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta

			if chunk.Choices[0].FinishReason != nil {
				fr := *chunk.Choices[0].FinishReason
				switch fr {
				case "length":
					finishReason = "max_tokens"
				case "content_filter":
					finishReason = "content_filter"
				case "tool_calls", "function_call":
					finishReason = "tool_use"
				default:
					finishReason = "end_turn"
				}
			}

			// Handle tool calls
			if len(delta.ToolCalls) > 0 {
				for _, tc := range delta.ToolCalls {
					toolIdx := tc.Index

					if currentBlockType != "tool" {
						if currentBlockType != "" {
							sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
							flusher.Flush()
						}
						currentBlockType = "tool"
					}

					if tc.Id != "" {
						if existingIdx, exists := toolIndexMap[toolIdx]; exists {
							sendEvent(w, "content_block_stop", map[string]interface{}{"index": existingIdx})
							flusher.Flush()
						}
						currentBlockIndex++
						toolIndexMap[toolIdx] = currentBlockIndex
						sendEvent(w, "content_block_start", map[string]interface{}{
							"index": currentBlockIndex,
							"content_block": map[string]interface{}{
								"type": "tool_use", "id": tc.Id, "name": tc.Function.Name, "input": map[string]string{},
							},
						})
						flusher.Flush()
					}

					if tc.Function.Arguments != "" {
						toolArgBuf[toolIdx] += tc.Function.Arguments

						if toolSent[toolIdx] {
							continue // already sent full JSON, ignore further fragments
						}

						if !toolFirstResolved[toolIdx] {
							var tmp map[string]interface{}
							if err := json.Unmarshal([]byte(toolArgBuf[toolIdx]), &tmp); err == nil {
								toolFirstResolved[toolIdx] = true
								clean := unwrapToolInput(tmp)
								if !reflect.DeepEqual(tmp, clean) {
									toolNested[toolIdx] = true
									// nested: we'll send nothing until the end
								} else {
									// not nested, send the accumulated buffer as first delta
									blockIdx := toolIndexMap[toolIdx]
									sendEvent(w, "content_block_delta", map[string]interface{}{
										"index": blockIdx,
										"delta": map[string]interface{}{
											"type": "input_json_delta", "partial_json": toolArgBuf[toolIdx],
										},
									})
									flusher.Flush()
									toolSent[toolIdx] = true
								}
							}
						} else if toolNested[toolIdx] {
							// nested and not yet sent, do nothing (buffer at end)
						} else {
							// already resolved and not nested: send the new fragment
							blockIdx := toolIndexMap[toolIdx]
							sendEvent(w, "content_block_delta", map[string]interface{}{
								"index": blockIdx,
								"delta": map[string]interface{}{
									"type": "input_json_delta", "partial_json": tc.Function.Arguments,
								},
							})
							flusher.Flush()
						}
					}
				}
				continue
			}

			// Handle thinking/reasoning content
			if delta.ReasoningContent != "" {
				if currentBlockType != "thinking" {
					if currentBlockType != "" {
						sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
						flusher.Flush()
					}
					currentBlockIndex++
					currentBlockType = "thinking"
					sendEvent(w, "content_block_start", map[string]interface{}{
						"index": currentBlockIndex,
						"content_block": map[string]string{"type": "thinking", "thinking": ""},
					})
					flusher.Flush()
				}
				sendEvent(w, "content_block_delta", map[string]interface{}{
					"index": currentBlockIndex,
					"delta": map[string]interface{}{
						"type": "thinking_delta", "thinking": delta.ReasoningContent,
					},
				})
				flusher.Flush()
				continue
			}

			// Handle text content
			var content string
			if len(delta.Content) > 0 {
				json.Unmarshal(delta.Content, &content)
			} else if delta.Refusal != "" {
				content = fmt.Sprintf("\n[Refusal: %s]\n", delta.Refusal)
			}
			if content != "" {
				if currentBlockType != "text" {
					if currentBlockType != "" {
						sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
						flusher.Flush()
					}
					currentBlockIndex++
					currentBlockType = "text"
					sendEvent(w, "content_block_start", map[string]interface{}{
						"index": currentBlockIndex,
						"content_block": map[string]string{"type": "text", "text": ""},
					})
					flusher.Flush()
				}
				sendEvent(w, "content_block_delta", map[string]interface{}{
					"index": currentBlockIndex,
					"delta": map[string]interface{}{"type": "text_delta", "text": content},
				})
				flusher.Flush()
			}
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}

	// Stream ended: send final tool call blocks for any nested tools that haven't been sent
	if currentBlockType == "tool" && len(toolIndexMap) > 0 {
		// Sort indices to ensure deterministic order
		var sortedIndices []int
		for idx := range toolIndexMap {
			sortedIndices = append(sortedIndices, idx)
		}
		sort.Ints(sortedIndices)
		for _, toolIdx := range sortedIndices {
			blockIdx := toolIndexMap[toolIdx]
			if !toolSent[toolIdx] {
				buf, has := toolArgBuf[toolIdx]
				if !has {
					buf = ""
				}
				if !toolFirstResolved[toolIdx] {
					// never resolved, send raw buffer
					sendEvent(w, "content_block_delta", map[string]interface{}{
						"index": blockIdx,
						"delta": map[string]interface{}{
							"type": "input_json_delta", "partial_json": buf,
						},
					})
				} else if toolNested[toolIdx] {
					// nested tool: send corrected full JSON
					var rawMap map[string]interface{}
					if err := json.Unmarshal([]byte(buf), &rawMap); err == nil {
						clean := unwrapToolInput(rawMap)
						cleanJson, _ := json.Marshal(clean)
						sendEvent(w, "content_block_delta", map[string]interface{}{
							"index": blockIdx,
							"delta": map[string]interface{}{
								"type": "input_json_delta", "partial_json": string(cleanJson),
							},
						})
					}
				}
			}
			sendEvent(w, "content_block_stop", map[string]interface{}{"index": blockIdx})
			flusher.Flush()
		}
	} else if currentBlockType != "" {
		sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
		flusher.Flush()
	} else {
		// no content at all - send empty text block
		sendEvent(w, "content_block_start", map[string]interface{}{
			"index": 0, "content_block": map[string]string{"type": "text", "text": ""},
		})
		sendEvent(w, "content_block_stop", map[string]interface{}{"index": 0})
		flusher.Flush()
	}

	streamErr := scanner.Err()
	if streamErr != nil {
		log.Printf("[STR] Stream error for %s: %v", model, streamErr)
		if finishReason == "end_turn" {
			finishReason = "error"
		}
	}

	usageData := map[string]int{"output_tokens": 0}
	if finalUsage != nil {
		usageData["output_tokens"] = finalUsage.CompletionTokens
		select {
		case usageLogChan <- UsageRecord{
			Time:       startTime,
			Service:    svcName,
			Model:      model,
			DurationMs: time.Since(startTime).Milliseconds(),
			Prompt:     finalUsage.PromptTokens,
			Completion: finalUsage.CompletionTokens,
			Total:      finalUsage.TotalTokens,
		}:
		default:
		}
		logCacheUsage(model, finalUsage)
	}

	sendEvent(w, "message_delta", map[string]interface{}{
		"delta": map[string]interface{}{"stop_reason": finishReason, "stop_sequence": nil},
		"usage": usageData,
	})
	flusher.Flush()

	sendEvent(w, "message_stop", map[string]interface{}{})
	flusher.Flush()
}

func sendEvent(w io.Writer, eventType string, data map[string]interface{}) {
	data["type"] = eventType

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("event: ")
	buf.WriteString(eventType)
	buf.WriteByte('\n')
	buf.WriteString("data: ")
	json.NewEncoder(buf).Encode(data)
	buf.WriteByte('\n')
	w.Write(buf.Bytes())
	bufferPool.Put(buf)
}

// --- Aggregation Logic ---
type StatKey struct {
	Date    string
	Service string
	Model   string
}
type StatValue struct {
	Requests   int
	Prompt     int
	Completion int
	Total      int
}

var statsMap = make(map[StatKey]*StatValue)
var statsMua sync.Mutex

const StatsFile = "usage_stats.csv"

func aggregatorWorker() {
	loadStats()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	dirty := false

	for {
		select {
		case record := <-usageLogChan:
			statsMua.Lock()
			date := record.Time.Format("2006-01-02")
			key := StatKey{Date: date, Service: record.Service, Model: record.Model}
			val, exists := statsMap[key]
			if !exists {
				val = &StatValue{}
				statsMap[key] = val
			}
			val.Requests++
			val.Prompt += record.Prompt
			val.Completion += record.Completion
			val.Total += record.Total
			statsMua.Unlock()
			dirty = true
		case <-ticker.C:
			if dirty {
				saveStats()
				dirty = false
			}
		}
	}
}

func loadStats() {
	f, err := os.Open(StatsFile)
	if err != nil {
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	if _, err := reader.Read(); err != nil {
		return
	}

	statsMua.Lock()
	defer statsMua.Unlock()

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) < 7 {
			continue
		}
		date := record[0]
		svc := record[1]
		model := record[2]
		var reqs, p, c, t int
		fmt.Sscanf(record[3], "%d", &reqs)
		fmt.Sscanf(record[4], "%d", &p)
		fmt.Sscanf(record[5], "%d", &c)
		fmt.Sscanf(record[6], "%d", &t)
		statsMap[StatKey{date, svc, model}] = &StatValue{
			Requests: reqs, Prompt: p, Completion: c, Total: t,
		}
	}
}

func saveStats() {
	statsMua.Lock()
	defer statsMua.Unlock()

	tempFile := StatsFile + ".tmp"
	f, err := os.Create(tempFile)
	if err != nil {
		log.Printf("[ERR] Failed to create temp stats file: %v", err)
		return
	}
	defer f.Close()

	writer := csv.NewWriter(f)
	writer.Write([]string{"Date", "Service", "Model", "Requests", "Prompt", "Completion", "Total"})

	var keys []StatKey
	for k := range statsMap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Date != keys[j].Date {
			return keys[i].Date > keys[j].Date
		}
		if keys[i].Service != keys[j].Service {
			return keys[i].Service < keys[j].Service
		}
		return keys[i].Model < keys[j].Model
	})

	for _, k := range keys {
		v := statsMap[k]
		writer.Write([]string{
			k.Date, k.Service, k.Model,
			fmt.Sprintf("%d", v.Requests),
			fmt.Sprintf("%d", v.Prompt),
			fmt.Sprintf("%d", v.Completion),
			fmt.Sprintf("%d", v.Total),
		})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Printf("[ERR] CSV Write Error: %v", err)
		return
	}
	f.Close()

	if err := os.Rename(tempFile, StatsFile); err != nil {
		log.Printf("[ERR] Failed to rename stats file: %v", err)
		os.Remove(tempFile)
	}
}