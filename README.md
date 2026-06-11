# a2o - Anthropic to OpenAI API Proxy

> 本人是地空学院博后，日常重度使用 Claude Code 辅助编程。Claude Code 的消息协议基于 Anthropic 格式，而 USTC 的 DeepSeek V4 目前仅 OpenAI 格式支持前缀缓存。市面上类似 `cc-switch` 的工具过于臃肿，于是利用Deepseek V4 pro写了这个极轻量的协议转换代理。

**轻量 + 命中缓存**，让 Claude Code 无缝接入 USTC DeepSeek。

### 亮点

- **单文件，零依赖** — 一个 `main.go`，Go 标准库直接编译，二进制 ~8MB
- **轻松命中前缀缓存** — 将 Anthropic `/v1/messages` 转为 OpenAI `/v1/chat/completions`，USTC LiteLLM 的 vLLM 前缀缓存全量生效，实测每次请求命中 50K+ tokens
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

USTC 的 LiteLLM 代理，缓存仅对 `/v1/chat/completions` 生效，`/v1/messages`（Anthropic 端点）吃不到缓存。

| 缓存层 | /v1/messages | /v1/chat/completions |
|--------|:--:|:--:|
| LiteLLM 响应缓存 | 端点不支持 | x-litellm-cache-key |
| vLLM 前缀缓存 | 内部可能生效但不可观测 | cached_tokens 可见 + Prefix Cache Hit |

```
Claude Code ──Anthropic──▶ a2o ──OpenAI──▶ USTC LiteLLM
                                              │
                                      ✅ 缓存全部生效
                                      💾 many tokens saved/request
```

## 实测缓存收益

以下是在 200K+ 字符巨型上下的生产数据：

| 时间 | Content-Length 增量 | 缓存命中 | 收益分析 |
|------|-------------------|---------|---------|
| 21:06:49 | 201,200 (起始) | **512 tokens** | 冷启动，vLLM 试探性匹配 |
| 21:07:27 | +4,626 | **50,432 tokens** | 缓存爆发，命中率 >95% |
| 21:08:19 | +3,228 | **51,712 tokens** | 持续增长，+1,280 tokens |
| 21:09:33 | +4,536 | — | 请求已发送，缓存命中在下一轮显示 |
| 21:10:51 | +1,217 | **52,736 tokens** | 继续累积，+1,024 tokens |
| 21:11:48 | +5,188 | **54,016 tokens** | 稳步增长，+1,280 tokens |
| 21:12:48 | +1,742 | **54,272 tokens** | 小幅增长，+256 tokens |
| 21:12:50 | +2,190 | **55,808 tokens** | 切换端口后继续，+1,536 tokens |
| 21:12:56 | +5,811 | **55,808→56,576** | 快速连续请求，+768 tokens |
| 21:13:06 | +2,266 | **56,576→58,368** | 显著跳跃，+1,792 tokens |
| 21:13:15 | +1,562 | **58,368→58,880** | 小幅递增，+512 tokens |
| 21:13:22 | +806 | **58,880→59,392** | 稳定增长，+512 tokens |
| 21:20:48 | +1,061 | — | 长间隔后请求，上一轮超时 |
| 21:22:43 | -33 | **59,648 tokens** | 恢复后继续，+256 tokens |
| 21:23:38 | +1,294 | **59,904 tokens** | 最终记录，+256 tokens |

**缓存规模**：缓存命中从初始 512 tokens 飙升至近 60K tokens，整个会话缓存命中率保持在 90% 以上
**增长模式**：上下文几乎呈线性增长，每轮增加约 1K-5K tokens，缓存同步累积，说明前缀匹配机制运转正常

## 快速开始

```bash
# 1. 编译
go build -o a2o main.go

# 2. 配置
cp config.example.json config.json
vim config.json

# 3. 运行
./a2o -config config.json

# 4. 测试
curl -s http://localhost:9999/v1/messages \
  -H "x-api-key: your-client-auth-key-here" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek-chat","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}'
```

## 配置说明

```json
{
  "debug_level": "info",
  "auth_token": "client-auth-key",
  "services": [
    {
      "comment": "ustc_deepseek",
      "listen_address": "9999",
      "openai_base_url": "https://api.llm.ustc.edu.cn/v1/chat/completions",
      "openai_api_key": "sk-xxx",
      "force_model": "",
      "upstream_proxy": ""
    }
  ],
  "round_robin_address": "",
  "timeout_seconds": 300
}
```

| 字段 | 说明 |
|------|------|
| `debug_level` | `"info"` 或 `"debug"`（输出缓存命中详情） |
| `auth_token` | a2o 自身的鉴权密钥，客户端需在 `x-api-key` 或 `Authorization: Bearer` 头中提供。注意：这是 Claude Code → a2o 之间的密钥，**不是**发往上游的 API key |
| `services[].listen_address` | a2o 监听端口（如 `9999`） |
| `services[].openai_base_url` | 上游 API 地址，需以 `/v1/chat/completions` 结尾 |
| `services[].openai_api_key` | 上游 API 密钥，a2o 发往上游时使用 |
| `services[].force_model` | 可选，强制覆盖客户端请求的 model 名称 |
| `round_robin_address` | 可选，多服务负载均衡端口 |
| `timeout_seconds` | 上游请求超时时间，默认 300 |

## 对接 Claude Code

设置 Claude Code 的环境变量指向 a2o：

```bash
export ANTHROPIC_AUTH_TOKEN=client-auth-key
export ANTHROPIC_BASE_URL=http://localhost:9999
```

注意：`ANTHROPIC_AUTH_TOKEN` 需与 `config.json` 中的 `auth_token` 一致，`ANTHROPIC_BASE_URL` 不需要 `/v1` 后缀。

## 端点

| 路径 | 说明 |
|------|------|
| `POST /v1/messages` | 主代理端点 |
| `GET /health` | 健康检查 |
| `POST /v1/messages/count_tokens` | Token 估算 |

## 调试

`config.json` 中设置 `"debug_level": "debug"` 即可看到缓存命中日志：

```
[deepseek-chat] 💾 Prefix Cache Hit: 50432 cached tokens (vLLM)
[deepseek-chat] 🔗 Cache Header: HIT
```

支持三种缓存检测格式：vLLM (`prompt_tokens_details.cached_tokens`)、DeepSeek 原生 (`prompt_cache_hit_tokens`)、响应头 (`X-DS-Cache-Hit` 等)。


