package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

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

	toolIndexMap := make(map[int]int)  // openai tool index -> anthropic block index
	toolArgBuf := make(map[int]string) // buffered arguments per tool index
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
						"index":         currentBlockIndex,
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
						"index":         currentBlockIndex,
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
