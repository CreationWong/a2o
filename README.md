# a2o - Anthropic to OpenAI API Proxy

> Forked from [inlizard](https://git.ustc.edu.cn/largeoyos/ai/-/tree/main/shares/inlizard) · Dockerized with env-var config support.
>
> Latest version: **v1.1.0**

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
# 从 Docker Hub 拉取
docker run -d --name a2o -p 9999:9999 \
  -e OPENAI_BASE_URL="https://your-api-service.com/v1/chat/completions" \
  -e OPENAI_API_KEY="sk-xxx" \
  -e AUTH_TOKEN="your-client-auth-key" \
  creationwong/a2o

# 或本地构建
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
| `FORCE_MODEL` | 强制覆盖 model 名称；支持 `上游id:下游id,ds:deepseek` 映射语法 | 不覆盖 |
| `UPSTREAM_PROXY` | 上游代理地址 | 无 |
| `ROUND_ROBIN_ADDRESS` | 多服务负载均衡端口 | 不开启 |
| `SERVICE_COMMENT` | 服务备注名；作为 `/v1/models` 返回的供应商名 | `default` |

`FORCE_MODEL` 映射示例：

```bash
# 客户端看到 deepseek，请求 deepseek 时转发给上游 ds
FORCE_MODEL="ds:deepseek"

# 多模型映射
FORCE_MODEL="ds:deepseek,gpt-4o:openai"
```

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
| `POST /v1/messages` | **主代理端点** — 接收 Anthropic 格式请求，转换为 OpenAI 格式转发 |
| `GET /v1/models` | 模型列表（FORCE_MODEL 指定时直接返回；否则透传上游） |
| `GET /models` | `/v1/models` 的别名 |
| `GET /health` | 健康检查 |
| `POST /v1/messages/count_tokens` | Token 估算 |

## API 格式转换

a2o 接收 **Anthropic Messages API** 格式的请求，转换为 **OpenAI Chat Completions** 格式发出，再将上游响应转换回 Anthropic 格式返回。

### 请求 — Anthropic → OpenAI 字段映射

| Anthropic 字段 | 转换方式 | OpenAI 目标 |
|----------------|----------|------------|
| `model` | 透传（FORCE_MODEL 指定则覆盖） | `model` |
| `messages[]` (role: user) | 按顺序转换，保持 text / image / tool_result 顺序 | `user` / `tool` 消息 |
| `messages[]` (role: assistant) | 拆分为 `reasoning_content` + text + `tool_calls` | `assistant` 消息 |
| `system` | 转为首条 `system` 消息 | `messages[0].role = "system"` |
| `max_tokens` | 透传 | `max_tokens` |
| `stop_sequences` | 透传 | `stop` |
| `stream` | 透传，自动添加 `stream_options: {include_usage: true}` | `stream` |
| `temperature` | 透传 | `temperature` |
| `top_p` | 透传 | `top_p` |
| `tools[]` | `name` / `description` / `input_schema` → `function` 格式 | `tools[].function` |
| `tool_choice` | 支持 `auto` / `any` → `required` / `tool` → `{type:"function", function:{name}}` | `tool_choice` |
| `metadata.user_id` | 透传 | `user` |

### 响应 — OpenAI → Anthropic 字段映射

| OpenAI 字段 | 转换方式 | Anthropic 目标 |
|-------------|----------|----------------|
| `choices[0].message.reasoning_content` | → `thinking` 类型 content block | `content[].type = "thinking"` |
| `choices[0].message.content` | → `text` 类型 content block | `content[].type = "text"` |
| `choices[0].message.tool_calls` | → `tool_use` 类型 content block | `content[].type = "tool_use"` |
| `choices[0].finish_reason` | `stop` → `end_turn`, `length` → `max_tokens`, `tool_calls` → `tool_use` | `stop_reason` |
| `usage` | 透传 | `usage` |

### 流式响应事件流

a2o 将 OpenAI SSE 流逐帧转换为 Anthropic SSE 格式：

```
OpenAI data chunk → Anthropic event type
────────────────────────────────────────
delta.content       → content_block_delta (text_delta)
delta.reasoning_content → content_block_delta (thinking_delta)
delta.tool_calls    → content_block_start / content_block_delta (input_json_delta)
finish_reason       → message_delta (stop_reason)
usage               → message_delta (usage)
[DONE]              → message_stop
```

### 消息顺序保持

Anthropic 允许在一条消息中交错排列不同 content block 类型（如 text → tool_use → text），a2o 在转换时按以下策略保持顺序：

- **user 消息**：text / image 合并为 `user` 条消息，`tool_result` 转为独立的 `tool` 消息
- **assistant 消息**：按 text / thinking / tool_use 顺序拆分为多个 `assistant` 消息，确保 OpenAI 端正确辨识
- **tool_use input 解嵌套**：自动检测并解开 `{"arguments": {...}}` 或 `{"arguments": "..."}` 嵌套结构

### 支持的 Content Block 类型

| 类型 | 上游 → a2o | a2o → 上游 |
|------|-----------|-----------|
| text | ✅ 接收 | ✅ 返回 |
| thinking | ✅ 接收 | ✅ 返回 |
| tool_use | ✅ 接收 | ✅ 返回 |
| tool_result | ✅ 接收 | — |
| image (base64 / URL) | ✅ 接收 | ✅ 返回 |

## 调试

设置 `DEBUG_LEVEL=debug` 即可看到缓存命中日志：

```
[deepseek-chat] 💾 Prefix Cache Hit: 50432 cached tokens (vLLM)
[deepseek-chat] 🔗 Cache Header: HIT
```

支持三种缓存检测格式：vLLM (`prompt_tokens_details.cached_tokens`)、DeepSeek 原生 (`prompt_cache_hit_tokens`)、响应头 (`X-DS-Cache-Hit` 等)。


