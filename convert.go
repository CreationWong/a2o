package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// convertToOpenAI now preserves content block order by splitting assistant messages
// with interleaved text/tool_use into multiple messages, and user messages with
// interleaved tool_results into proper ordered sequences.
func convertToOpenAI(ant *AnthropicRequest, targetModel string) (*OpenAIRequest, error) {
	oai := &OpenAIRequest{
		Model:       targetModel,
		MaxTokens:   ant.MaxTokens,
		Stream:      ant.Stream,
		Temperature: ant.Temperature,
		TopP:        ant.TopP,
		Stop:        ant.StopSequences,
		Messages:    []OpenAIMessage{},
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
	Type      string                 // "text", "thinking", "tool_use", "tool_result", "image"
	Text      string                 // for text/thinking
	Source    map[string]interface{} // for image
	Id        string                 // for tool_use/tool_result
	Name      string                 // for tool_use
	Input     map[string]interface{} // for tool_use
	ToolUseId string                 // for tool_result
	IsError   bool                   // for tool_result
	Content   interface{}            // raw tool_result content
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
