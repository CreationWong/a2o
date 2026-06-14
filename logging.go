package main

import "log"

func logDebug(format string, v ...interface{}) {
	if config.DebugLevel == "debug" {
		log.Printf(format, v...)
	}
}

func logCacheInfo(listenAddr string, headers httpHeader, usage *OpenAIUsage) {
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

type httpHeader interface {
	Get(string) string
}
