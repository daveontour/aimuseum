package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// LocalAIProvider calls a llama.cpp server (or any OpenAI-compatible server)
// via the /v1/chat/completions endpoint with tool-calling support.
type LocalAIProvider struct {
	baseURL   string
	apiKey    string
	modelName string
}

// NewLocalAIProvider creates a LocalAIProvider. Returns nil if baseURL is empty.
func NewLocalAIProvider(baseURL, apiKey, modelName string) *LocalAIProvider {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	if modelName == "" {
		modelName = "local-model"
	}
	return &LocalAIProvider{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    apiKey,
		modelName: modelName,
	}
}

func (p *LocalAIProvider) IsAvailable() bool { return p != nil }

// SimpleGenerate sends a prompt to the local AI without tools.
// Used for summarization and other tasks that don't need tool-calling.
func (p *LocalAIProvider) SimpleGenerate(ctx context.Context, prompt string) (string, *LLMUsage, error) {
	if p == nil || p.baseURL == "" {
		return "", nil, fmt.Errorf("localai: not configured")
	}
	body := map[string]any{
		"model":       p.modelName,
		"temperature": 0.2,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	resp, err := localaiPost(ctx, p.baseURL, p.apiKey, body)
	if err != nil {
		return "", nil, err
	}
	usage := localaiUsageFromResponse(resp, p.modelName)
	text := localaiExtractText(resp)
	return strings.TrimSpace(text), usage, nil
}

func localaiUsageFromResponse(resp map[string]any, modelName string) *LLMUsage {
	in, out := 0, 0
	if u, ok := resp["usage"].(map[string]any); ok {
		if v, ok := u["prompt_tokens"].(float64); ok {
			in = int(v)
		}
		if v, ok := u["completion_tokens"].(float64); ok {
			out = int(v)
		}
	}
	if in == 0 && out == 0 {
		return nil
	}
	return &LLMUsage{Provider: "localai", Model: modelName, InputTokens: in, OutputTokens: out}
}

// localaiExtractText pulls the assistant content string from the first choice.
func localaiExtractText(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}

// localaiExtractToolCalls returns tool_calls from the first choice's message, if any.
func localaiExtractToolCalls(resp map[string]any) []map[string]any {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	raw, _ := msg["tool_calls"].([]any)
	if len(raw) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, tc := range raw {
		if m, ok := tc.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// localaiFinishReason returns the finish_reason string for the first choice.
func localaiFinishReason(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	reason, _ := choice["finish_reason"].(string)
	return reason
}

// GenerateResponse calls the local AI with a tool-calling loop.
func (p *LocalAIProvider) GenerateResponse(
	ctx context.Context,
	req GenerateRequest,
	systemPrompt string,
	history []ConvTurn,
	executor ToolExecutor,
	toolDecls *[]map[string]any,
) (GenerateResult, error) {
	if executor == nil {
		empty := []map[string]any{}
		toolDecls = &empty
	}
	exec := executor
	if exec == nil {
		exec = func(_ context.Context, name string, _ map[string]any) (map[string]any, error) {
			return map[string]any{"error": "tool execution not available: " + name}, nil
		}
	}

	var defs []map[string]any
	if toolDecls == nil {
		defs = toolDefinitions()
	} else {
		defs = *toolDecls
	}

	// Convert tool definitions to OpenAI format.
	var openAITools []map[string]any
	for _, td := range defs {
		openAITools = append(openAITools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        td["name"],
				"description": td["description"],
				"parameters":  td["parameters"],
			},
		})
	}

	messages := buildLocalAIMessages(req, history, systemPrompt)
	funcCallsMade := []map[string]any{}
	inputTokens := 0
	outputTokens := 0

	for iter := 0; iter < maxToolCallIterations; iter++ {
		body := map[string]any{
			"model":       p.modelName,
			"temperature": req.Temperature,
			"messages":    messages,
		}
		if len(openAITools) > 0 {
			body["tools"] = openAITools
			body["tool_choice"] = "auto"
		}

		resp, err := localaiPost(ctx, p.baseURL, p.apiKey, body)
		if err != nil {
			return GenerateResult{}, fmt.Errorf("localai: %w", err)
		}

		// Accumulate token usage.
		if u, ok := resp["usage"].(map[string]any); ok {
			if v, ok := u["prompt_tokens"].(float64); ok {
				inputTokens += int(v)
			}
			if v, ok := u["completion_tokens"].(float64); ok {
				outputTokens += int(v)
			}
		}

		finishReason := localaiFinishReason(resp)
		toolCalls := localaiExtractToolCalls(resp)

		if finishReason != "tool_calls" || len(toolCalls) == 0 {
			responseText := strings.TrimSpace(localaiExtractText(resp))
			if responseText == "" {
				responseText = "I apologize, but I couldn't generate a response."
			}
			plainText, embeddedElements := extractEmbeddedJSON(responseText)
			metadata := map[string]any{
				"referenced_files": []any{},
				"function_calls":   funcCallsMade,
				"input_tokens":     inputTokens,
				"output_tokens":    outputTokens,
				"total_tokens":     inputTokens + outputTokens,
				"provider":         "localai",
				"model":            p.modelName,
			}
			if len(embeddedElements) > 0 {
				metadata["embedded_json"] = embeddedElements
			}
			metaJSON, _ := json.Marshal(metadata)
			return GenerateResult{
				PlainText:    plainText,
				MetadataJSON: string(metaJSON),
				Voice:        req.Voice,
				Usage: &LLMUsage{
					Provider: "localai", Model: p.modelName,
					InputTokens: inputTokens, OutputTokens: outputTokens,
				},
			}, nil
		}

		// Append the assistant message (with tool_calls) to history.
		choices, _ := resp["choices"].([]any)
		var assistantMsg map[string]any
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			assistantMsg, _ = choice["message"].(map[string]any)
		}
		if assistantMsg == nil {
			assistantMsg = map[string]any{"role": "assistant", "tool_calls": toolCalls}
		}
		messages = append(messages, assistantMsg)

		// Execute each tool call and collect results.
		for _, tc := range toolCalls {
			callID, _ := tc["id"].(string)
			fn, _ := tc["function"].(map[string]any)
			toolName, _ := fn["name"].(string)
			argsStr, _ := fn["arguments"].(string)

			var toolInput map[string]any
			if argsStr != "" {
				_ = json.Unmarshal([]byte(argsStr), &toolInput)
			}
			if toolInput == nil {
				toolInput = map[string]any{}
			}

			funcCallsMade = append(funcCallsMade, map[string]any{
				"name":      toolName,
				"arguments": toolInput,
				"iteration": iter + 1,
			})

			result, err := exec(ctx, toolName, toolInput)
			if err != nil {
				result = map[string]any{"error": err.Error()}
			}
			resultJSON, _ := json.Marshal(result)

			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      string(resultJSON),
			})
		}
	}

	return GenerateResult{}, fmt.Errorf("localai: exceeded max tool-calling iterations")
}

func buildLocalAIMessages(req GenerateRequest, history []ConvTurn, systemPrompt string) []map[string]any {
	var messages []map[string]any

	if systemPrompt != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	recent := history
	if len(recent) > 20 {
		recent = recent[len(recent)-20:]
	}
	for _, turn := range recent {
		messages = append(messages,
			map[string]any{"role": "user", "content": turn.UserInput},
			map[string]any{"role": "assistant", "content": turn.ResponseText},
		)
	}

	var parts []string
	if req.Voice == "owner" {
		parts = append(parts, "IMPORTANT: Respond in the first person voice as the owner of the subject's life.")
		parts = append(parts, fmt.Sprintf("IMPORTANT: Your current mood is %s", req.Mood))
		if req.PsychProfile != nil && *req.PsychProfile != "" {
			parts = append(parts, fmt.Sprintf("IMPORTANT: Respond consistent with your psychological profile: %s", *req.PsychProfile))
		}
		if req.WritingStyle != nil && *req.WritingStyle != "" {
			parts = append(parts, fmt.Sprintf("IMPORTANT: Respond consistent with your writing style: %s", *req.WritingStyle))
		}
	}
	if req.CompanionMode {
		parts = append(parts, "IMPORTANT: You are in companion mode. Respond conversationally as a friend, not as an assistant.")
	}
	if len(parts) > 0 {
		parts = append(parts, "\nUser input:\n"+req.UserInput)
	} else {
		parts = []string{req.UserInput}
	}
	messages = append(messages, map[string]any{"role": "user", "content": strings.Join(parts, "\n")})
	return messages
}

func localaiPost(ctx context.Context, baseURL, apiKey string, body map[string]any) (map[string]any, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := baseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LocalAI API %d: %s", resp.StatusCode, string(data))
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}
