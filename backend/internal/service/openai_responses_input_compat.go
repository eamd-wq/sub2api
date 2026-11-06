package service

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIResponsesInputTextMaxChars = 10000000

// normalizeOpenAIResponsesMessageTypesForAccount 为显式启用的严格上游补齐消息默认类型。
// 原始 JSON 项逐段复制，保留文本、工具配对、phase 和数字精度；不重建请求历史。
func normalizeOpenAIResponsesMessageTypesForAccount(account *Account, body []byte) ([]byte, error) {
	if account == nil || !account.IsOpenAIApiKey() || account.Extra[openai_compat.ExtraKeyResponsesExplicitMessageType] != true || !gjson.ValidBytes(body) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	needsType := func(item gjson.Result) bool {
		if !item.IsObject() || item.Get("type").Exists() {
			return false
		}
		switch item.Get("role").String() {
		case "user", "assistant", "system", "developer":
		default:
			return false
		}
		content := item.Get("content")
		return content.Type == gjson.String || content.IsArray()
	}
	modified := false
	input.ForEach(func(_, item gjson.Result) bool {
		modified = needsType(item)
		return !modified
	})
	if !modified {
		return body, nil
	}
	var normalized bytes.Buffer
	normalized.Grow(len(input.Raw) + 64)
	normalized.WriteByte('[')
	first := true
	input.ForEach(func(_, item gjson.Result) bool {
		if !first {
			normalized.WriteByte(',')
		}
		first = false
		if needsType(item) {
			normalized.WriteString(item.Raw[:len(item.Raw)-1])
			normalized.WriteString(`,"type":"message"}`)
		} else {
			normalized.WriteString(item.Raw)
		}
		return true
	})
	normalized.WriteByte(']')
	result, err := sjson.SetRawBytes(body, "input", normalized.Bytes())
	if err != nil {
		return nil, fmt.Errorf("normalize Responses message types: %w", err)
	}
	return result, nil
}

// sanitizeOpenAIResponsesOrphanToolOutputs removes tool-output items that have
// no matching call or item reference anywhere in the current input. Named
// function outputs without a call ID are standalone inputs, not orphan results.
func sanitizeOpenAIResponsesOrphanToolOutputs(reqBody map[string]any, input []any, hasPreviousResponseID bool) bool {
	if len(input) == 0 || hasPreviousResponseID {
		return false
	}

	toolCallIDs := make(map[string]struct{}, len(input))
	referenceIDs := make(map[string]struct{}, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.TrimSpace(firstNonEmptyString(item["type"]))
		if itemType == "item_reference" {
			if id := strings.TrimSpace(firstNonEmptyString(item["id"])); id != "" {
				referenceIDs[id] = struct{}{}
			}
			continue
		}
		if !isCodexToolCallContextItemType(itemType) {
			continue
		}
		if id := strings.TrimSpace(firstNonEmptyString(item["call_id"], item["id"])); id != "" {
			toolCallIDs[id] = struct{}{}
		}
	}

	modified := false
	normalized := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || !isCodexToolCallOutputItemType(strings.TrimSpace(firstNonEmptyString(item["type"]))) {
			normalized = append(normalized, rawItem)
			continue
		}

		callID := strings.TrimSpace(firstNonEmptyString(item["call_id"]))
		// Codex sends externally supplied inputs (such as task delegation) as
		// named function outputs without a preceding function call. Preserve
		// their native type, namespace and output instead of silently dropping
		// the request that started the turn.
		if callID == "" && strings.TrimSpace(firstNonEmptyString(item["type"])) == "function_call_output" &&
			strings.TrimSpace(firstNonEmptyString(item["name"])) != "" {
			normalized = append(normalized, rawItem)
			continue
		}
		_, hasToolCall := toolCallIDs[callID]
		_, hasReference := referenceIDs[callID]
		if callID != "" && (hasToolCall || hasReference) {
			normalized = append(normalized, rawItem)
			continue
		}

		modified = true
	}
	if !modified {
		return false
	}
	reqBody["input"] = normalized
	return true
}

func truncateOpenAIResponsesInputText(_ map[string]any) bool {
	// Do not silently rewrite client or tool output. If an upstream enforces a
	// text limit, forwarding the original value preserves its explicit error for
	// the client and the normal Ops error pipeline. This compatibility shim is
	// retained until the two callers can remove the old mutation hook together.
	return false
}

func openAIResponsesInputMayNeedTruncation(_ []byte) bool {
	// See truncateOpenAIResponsesInputText. Returning false also avoids decoding
	// very large bodies solely for a mutation that must not happen.
	return false
}
