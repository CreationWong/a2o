# a2o - Anthropic to OpenAI API Proxy

> Forked from [inlizard](https://git.ustc.edu.cn/largeoyos/ai/-/tree/main/shares/inlizard) · Dockerized with env-var config support.

**轻量 + 命中缓存**，让 Claude Code 无缝接入仅支持 OpenAI 格式的 API 服务。

### 亮点

- **Docker 化部署** — 基于 `golang:alpine` 构建，镜像 ~20MB
- **轻松命中前缀缓存** — 将 Anthropic `/v1/messages` 转为 OpenAI `/v1/chat/completions`，让仅支持 OpenAI 格式的 API 也能享受 vLLM 前缀缓存，实测每次请求命中 50K+ tokens
- **10ms 开销** — 本地转换只多一层网络，换来：
  - 50K+ system prompt tokens 免计算（前缀缓存命中）
  - 首次请求 18s，后续有缓存后 3s 内完成响应
  - 超 95% 的计算被缓存跳过，编程效率翻倍
- 支持流式/非流式、tool calling、thinking/reasoning
- 支持多服务负载均衡、上游代理

### 兼容性

| 特性 | 状态 |
|------|:--:|
| 流式/非流式响应 | ✅ |
| Tool calling (tools + tool_use + tool_result) | ✅ |
| Thinking / reasoning blocks | ✅ |
| 前缀缓存透传 | ✅ |

## 为什么需要 a2o

部分 API 代理的缓存仅对 `/v1/chat/completions` 生效，`/v1/messages`（Anthropic 端点）吃不到缓存。

| 缓存层 | /v1/messages | /v1/chat/completions |
|--------|:--:|:--:|
| LiteLLM 响应缓存 | 端点不支持 | x-litellm-cache-key |
| vLLM 前缀缓存 | 内部可能生效但不可观测 | cached_tokens 可见 + Prefix Cache Hit |

```
Claude Code ──Anthropic──▶ a2o ──OpenAI──▶ API Service
                                              │
                                      ✅ 缓存全部生效
                                      💾 many tokens saved/request
```

## 快速开始

```bash
docker run -d --name a2o -p 9999:9999 \
  -e OPENAI_BASE_URL="https://your-api-service.com/v1/chat/completions" \
  -e OPENAI_API_KEY="sk-xxx" \
  -e AUTH_TOKEN="your-client-auth-key" \
  -e TZ=Asia/Shanghai \
  ghcr.io/creationwong/a2o
```

或本地构建：

```bash
docker build -t a2o .
docker run -d --name a2o -p 9999:9999 \
  -e OPENAI_BASE_URL="..." \
  -e OPENAI_API_KEY="sk-xxx" \
  a2o
```

验证：

```bash
curl -s http://localhost:9999/v1/messages \
  -H "x-api-key: your-client-auth-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek-chat","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}'
```

## 环境变量配置

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `OPENAI_BASE_URL` | **上游 API 地址（必填）** | — |
| `OPENAI_API_KEY` | **上游 API 密钥（必填）** | — |
| `LISTEN_ADDRESS` | 监听端口 | `9999` |
| `AUTH_TOKEN` | 客户端鉴权密钥 | 不鉴权 |
| `DEBUG_LEVEL` | 日志级别 `info` / `debug` | `info` |
| `TIMEOUT_SECONDS` | 上游超时 | `300` |
| `FORCE_MODEL` | 强制覆盖 model 名称 | 不覆盖 |
| `UPSTREAM_PROXY` | 上游代理地址 | 无 |
| `ROUND_ROBIN_ADDRESS` | 多服务负载均衡端口 | 不开启 |
| `SERVICE_COMMENT` | 服务备注名 | `default` |

## 对接 Claude Code

设置 Claude Code 的环境变量指向 a2o：

```bash
export ANTHROPIC_AUTH_TOKEN=client-auth-key
export ANTHROPIC_BASE_URL=http://localhost:9999
```

注意：`ANTHROPIC_AUTH_TOKEN` 需与 a2o 的 `AUTH_TOKEN` 一致，`ANTHROPIC_BASE_URL` 不需要 `/v1` 后缀。

## 端点

| 路径 | 说明 |
|------|------|
| `POST /v1/messages` | 主代理端点 |
| `GET /health` | 健康检查 |
| `POST /v1/messages/count_tokens` | Token 估算 |

## 调试

设置 `DEBUG_LEVEL=debug` 即可看到缓存命中日志：

```
[deepseek-chat] 💾 Prefix Cache Hit: 50432 cached tokens (vLLM)
[deepseek-chat] 🔗 Cache Header: HIT
```

支持三种缓存检测格式：vLLM (`prompt_tokens_details.cached_tokens`)、DeepSeek 原生 (`prompt_cache_hit_tokens`)、响应头 (`X-DS-Cache-Hit` 等)。


