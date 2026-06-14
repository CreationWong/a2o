package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

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
