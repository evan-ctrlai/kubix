package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"kubix/internal/config"
)

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolDefinition struct {
	Type     string         `json:"type"`
	Function map[string]any `json:"function"`
}

type Model interface {
	Complete(context.Context, []Message, []ToolDefinition) (Message, error)
}

// StructuredModel is an optional capability for a single, tool-free summary.
// Callers still validate the returned claims and their evidence references.
type StructuredModel interface {
	CompleteStructured(context.Context, []Message, json.RawMessage) (Message, error)
}

// ModelError contains only a safe user-facing explanation, never response bodies.
type ModelError struct{ Message string }

func (e *ModelError) Error() string { return e.Message }

type HTTPModel struct {
	settings config.ModelSettings
	client   *http.Client
}

func NewHTTPModel(settings config.ModelSettings) *HTTPModel {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // A shell proxy must not silently change the selected destination.
	return &HTTPModel{settings: settings, client: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (m *HTTPModel) Close() { m.client.CloseIdleConnections() }

func (m *HTTPModel) Complete(ctx context.Context, messages []Message, tools []ToolDefinition) (Message, error) {
	return m.complete(ctx, messages, tools, nil)
}

func (m *HTTPModel) CompleteStructured(ctx context.Context, messages []Message, schema json.RawMessage) (Message, error) {
	var object map[string]any
	if len(schema) == 0 || len(schema) > 16*1024 || json.Unmarshal(schema, &object) != nil || object == nil {
		return Message{}, &ModelError{"Structured response schema must be a JSON object within 16 KiB"}
	}
	return m.complete(ctx, messages, nil, schema)
}

func (m *HTTPModel) complete(ctx context.Context, messages []Message, tools []ToolDefinition, schema json.RawMessage) (Message, error) {
	var empty Message
	endpoint, err := url.Parse(m.settings.BaseURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return empty, &ModelError{"Model URL must be an HTTP(S) base URL without embedded credentials, query, or fragment"}
	}
	if m.settings.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.settings.Timeout)
		defer cancel()
	}
	payload := map[string]any{"model": m.settings.Model, "messages": messages, "temperature": 0.1, "max_tokens": 1800}
	if schema != nil {
		payload["temperature"] = 0
		payload["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "crashloop_summary", "strict": true, "schema": schema},
		}
	} else if len(tools) > 0 {
		payload["tools"], payload["tool_choice"], payload["parallel_tool_calls"] = tools, "auto", false
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return empty, &ModelError{"Unable to encode the model request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.settings.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return empty, &ModelError{"Invalid model endpoint"}
	}
	req.Header.Set("Content-Type", "application/json")
	if m.settings.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.settings.APIKey)
	}
	response, err := m.client.Do(req)
	if err != nil {
		return empty, requestError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return empty, &ModelError{fmt.Sprintf("Model endpoint returned HTTP %d", response.StatusCode)}
	}
	const responseLimit = 128 * 1024
	data, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return empty, requestError(ctx, err)
	}
	if len(data) > responseLimit {
		return empty, &ModelError{"Model response exceeded 128 KiB"}
	}
	var result struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Choices) == 0 || result.Choices[0].Message.Role != "assistant" {
		return empty, &ModelError{"Model endpoint returned an invalid chat-completion response"}
	}
	if result.Choices[0].FinishReason == "length" {
		return empty, &ModelError{"Model output hit its token limit; try a narrower question"}
	}
	return result.Choices[0].Message, nil
}

func requestError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &ModelError{"Model request timed out"}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return &ModelError{"Investigation cancelled"}
	}
	return &ModelError{"Cannot reach model endpoint; check the configured URL and server"}
}
