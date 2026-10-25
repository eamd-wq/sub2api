package service

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// shouldBridgeResponsesInputLimit 仅对显式声明上限的 API Key 账号处理超长 HTTP 请求。
// 不修改共享账号、不截断或摘要历史；回程复用现有 Responses 流式与工具调用转换。
func (s *OpenAIGatewayService) shouldBridgeResponsesInputLimit(c *gin.Context, account *Account, body []byte) (bool, error) {
	if account == nil || !account.IsOpenAIApiKey() || c == nil ||
		GetOpenAIClientTransport(c) == OpenAIClientTransportWS || openAIResponsesRequestPathSuffix(c) != "" {
		return false, nil
	}
	limit := account.getExtraInt(openai_compat.ExtraKeyResponsesInputItemLimit)
	input := gjson.GetBytes(body, "input")
	if limit <= 0 || !input.IsArray() {
		return false, nil
	}
	count := 0
	input.ForEach(func(_, _ gjson.Result) bool { count++; return true })
	if count <= limit {
		return false, nil
	}
	// 兼容桥原本允许跳过未知项、修复残缺工具历史；自动切换必须先拒绝这些有损情形。
	if err := validateResponsesInputLimitBridge(body); err != nil {
		message := fmt.Sprintf("Input context length exceeded: %d input items exceed the upstream limit of %d; automatic Chat conversion cannot preserve this request: %s", count, limit, err)
		setOpsUpstreamError(c, http.StatusBadRequest, message, "")
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type": "invalid_request_error", "code": "context_length_exceeded", "param": "input", "message": message,
		}})
		return false, fmt.Errorf("responses input limit: %w", err)
	}
	logger.L().Info("openai responses: input item limit uses Chat bridge",
		zap.Int64("account_id", account.ID), zap.Int("input_items", count), zap.Int("input_item_limit", limit))
	return true, nil
}

func validateResponsesInputLimitBridge(body []byte) error {
	if !json.Valid(body) {
		return fmt.Errorf("invalid JSON")
	}
	root := gjson.ParseBytes(body)
	for key, value := range root.Map() {
		if value.Type == gjson.Null {
			continue
		}
		switch key {
		case "model", "input", "instructions", "max_output_tokens", "temperature", "top_p", "stream",
			"tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "service_tier",
			"prompt_cache_key", "prompt_cache_options", "metadata", "include":
		case "store", "background":
			if value.Type != gjson.False {
				return fmt.Errorf("stored or background Responses require the native endpoint")
			}
		case "previous_response_id", "conversation", "prompt":
			return fmt.Errorf("server-side conversation state requires the native endpoint")
		case "truncation":
			if value.String() != "disabled" {
				return fmt.Errorf("automatic truncation requires the native endpoint")
			}
		default:
			return fmt.Errorf("an unsupported Responses request field requires the native endpoint")
		}
	}
	if root.Get("text.verbosity").Exists() {
		return fmt.Errorf("text verbosity requires the native endpoint")
	}
	for key := range root.Get("reasoning").Map() {
		if key != "effort" && key != "summary" {
			return fmt.Errorf("a reasoning option requires the native endpoint")
		}
	}
	choice := root.Get("tool_choice")
	if choice.Exists() && choice.Type != gjson.Null {
		switch choice.String() {
		case "auto", "none", "required":
		default:
			if !choice.IsObject() || (choice.Get("type").String() != "function" && choice.Get("type").String() != "custom") || choice.Get("name").String() == "" {
				return fmt.Errorf("the tool choice requires the native endpoint")
			}
		}
	}
	for _, include := range root.Get("include").Array() {
		if include.String() != "reasoning.encrypted_content" {
			return fmt.Errorf("the requested output expansion requires the native endpoint")
		}
	}
	if err := validateResponsesInputLimitTools(root.Get("tools")); err != nil {
		return err
	}
	// 检查完整的工具配对，防止原兼容桥的历史自愈逻辑丢弃调用或结果。
	pending := make(map[string]bool)
	seen := make(map[string]bool)
	for _, item := range root.Get("input").Array() {
		if item.Type == gjson.String {
			continue
		}
		switch item.Get("type").String() {
		case "", "message":
			if err := validateResponsesInputLimitContent(item.Get("content"), item.Get("role").String()); err != nil {
				return err
			}
		case "reasoning":
			_, text, _ := apicompat.ExtractResponsesReasoningItem(json.RawMessage(item.Raw))
			if text == "" && item.Get("encrypted_content").String() != "" {
				return fmt.Errorf("encrypted-only reasoning requires the native endpoint")
			}
		case "function_call", "custom_tool_call", "tool_search_call":
			id := item.Get("call_id").String()
			if id == "" || seen[id] {
				return fmt.Errorf("tool history contains missing or duplicate call IDs")
			}
			if item.Get("type").String() == "function_call" {
				args := item.Get("arguments")
				if args.Type != gjson.String || !json.Valid([]byte(args.Str)) {
					return fmt.Errorf("tool history contains incomplete function arguments")
				}
			}
			seen[id], pending[id] = true, true
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			id := item.Get("call_id").String()
			if !pending[id] {
				return fmt.Errorf("tool history contains an unpaired output")
			}
			delete(pending, id)
			if err := validateResponsesInputLimitTools(item.Get("tools")); err != nil {
				return err
			}
		case "additional_tools":
			if err := validateResponsesInputLimitTools(item.Get("tools")); err != nil {
				return err
			}
		default:
			return fmt.Errorf("an input item has no Chat equivalent")
		}
	}
	if len(pending) != 0 {
		return fmt.Errorf("tool history contains unanswered calls")
	}
	return nil
}

func validateResponsesInputLimitTools(tools gjson.Result) error {
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "function":
		case "custom":
			if format := tool.Get("format.type"); format.Exists() && format.String() != "text" {
				return fmt.Errorf("grammar-constrained custom tools require the native endpoint")
			}
		case "namespace":
			if err := validateResponsesInputLimitTools(tool.Get("tools")); err != nil {
				return err
			}
		case "tool_search":
			if tool.Get("execution").String() != "client" {
				return fmt.Errorf("server-executed tools require the native endpoint")
			}
		default:
			return fmt.Errorf("a declared tool requires the native endpoint")
		}
	}
	return nil
}

func validateResponsesInputLimitContent(content gjson.Result, role string) error {
	if content.Type == gjson.String || content.Type == gjson.Null {
		return nil
	}
	if !content.IsArray() {
		return fmt.Errorf("message content has no Chat equivalent")
	}
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "input_text", "output_text", "text":
		case "input_image", "image_url":
			if (role != "user" && role != "") || part.Get("image_url").String() == "" {
				return fmt.Errorf("an image cannot be preserved in Chat message content")
			}
		default:
			return fmt.Errorf("a content part requires the native endpoint")
		}
	}
	return nil
}
