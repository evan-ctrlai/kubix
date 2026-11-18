package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kubix/internal/config"
)

func TestHTTPModelRequestAndCredentials(t *testing.T) {
	var path, authorization string
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, authorization = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer server.Close()
	s := config.DefaultModelSettings()
	s.BaseURL = server.URL + "/v1"
	s.Model = "qwen"
	s.APIKey = "secret"
	m := NewHTTPModel(s)
	defer m.Close()
	result, err := m.Complete(context.Background(), []Message{{Role: "user", Content: "Hi"}}, toolDefinitions())
	if err != nil || result.Content != "hello" {
		t.Fatalf("%+v %v", result, err)
	}
	if path != "/v1/chat/completions" || authorization != "Bearer secret" || payload["model"] != "qwen" {
		t.Fatalf("wrong request: %s %s %+v", path, authorization, payload)
	}
	if payload["parallel_tool_calls"] != false || payload["tool_choice"] != "auto" {
		t.Fatal("missing tool configuration")
	}
}

func TestHTTPStructuredSummaryDoesNotChangeOrdinaryRequests(t *testing.T) {
	requests := make(chan map[string]any, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{}"}}]}`))
	}))
	defer server.Close()
	settings := config.DefaultModelSettings()
	settings.BaseURL = server.URL + "/v1"
	model := NewHTTPModel(settings)
	defer model.Close()
	var structured StructuredModel = model
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"citations":{"type":"array","items":{"type":"string","enum":["E1","E3"]}}},"required":["citations"]}`)
	messages := []Message{{Role: "user", Content: "Summarize the captured evidence."}}
	if _, err := structured.CompleteStructured(context.Background(), messages, schema); err != nil {
		t.Fatal(err)
	}
	payload := <-requests
	format, ok := payload["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" || payload["temperature"] != float64(0) || payload["max_tokens"] != float64(1800) {
		t.Fatalf("structured request settings: %#v", payload)
	}
	definition := format["json_schema"].(map[string]any)
	if definition["name"] != "crashloop_summary" || definition["strict"] != true {
		t.Fatalf("missing schema contract: %#v", definition)
	}
	var expectedSchema any
	_ = json.Unmarshal(schema, &expectedSchema)
	want, _ := json.Marshal(expectedSchema)
	got, _ := json.Marshal(definition["schema"])
	if string(got) != string(want) {
		t.Fatalf("schema was changed or encoded as a string: %s", got)
	}
	for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if _, exists := payload[key]; exists {
			t.Errorf("structured summary exposed %s", key)
		}
	}
	// Reuse the same instance: a structured request must not leave sticky state
	// that changes a subsequent plain answer or ordinary tool-calling turn.
	for _, withTools := range []bool{false, true} {
		var tools []ToolDefinition
		if withTools {
			tools = toolDefinitions()
		}
		if _, err := model.Complete(context.Background(), messages, tools); err != nil {
			t.Fatal(err)
		}
		ordinary := <-requests
		if _, exists := ordinary["response_format"]; exists || ordinary["temperature"] != 0.1 {
			t.Fatalf("ordinary request changed: %#v", ordinary)
		}
		_, hasTools := ordinary["tools"]
		if hasTools != withTools {
			t.Fatal("ordinary tool availability changed")
		}
		if withTools && (ordinary["tool_choice"] != "auto" || ordinary["parallel_tool_calls"] != false) {
			t.Fatal("ordinary tool policy changed")
		}
	}
}

func TestHTTPStructuredSummaryRejectsInvalidSchemaAndDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("unsupported format; password=do-not-expose"))
	}))
	defer server.Close()
	settings := config.DefaultModelSettings()
	settings.BaseURL = server.URL
	model := NewHTTPModel(settings)
	defer model.Close()
	for _, invalid := range []json.RawMessage{nil, []byte(`null`), []byte(`true`), []byte(`[]`), []byte(`{"broken"`), []byte(strings.Repeat(" ", 16*1024+1))} {
		if _, err := model.CompleteStructured(context.Background(), nil, invalid); err == nil {
			t.Fatalf("invalid schema accepted: %q", invalid)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid schema reached the model endpoint")
	}
	_, err := model.CompleteStructured(context.Background(), nil, json.RawMessage(`{"type":"object"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || strings.Contains(err.Error(), "do-not-expose") || calls.Load() != 1 {
		t.Fatalf("unsupported schema request retried or leaked response: %v, calls=%d", err, calls.Load())
	}
}

func TestHTTPModelRejectsBadResponsesWithoutLeakingBodies(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", 401, "api_key=do-not-expose", "HTTP 401"},
		{"empty_choices", 200, `{"choices":[]}`, "invalid chat-completion"},
		{"wrong_role", 200, `{"choices":[{"message":{"role":"system"}}]}`, "invalid chat-completion"},
		{"wrong_content_type", 200, `{"choices":[{"message":{"role":"assistant","content":{}}}]}`, "invalid chat-completion"},
		{"oversized", 200, strings.Repeat("x", 131073), "exceeded 128 KiB"},
		{"token_limit", 200, `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"partial"}}]}`, "token limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			s := config.DefaultModelSettings()
			s.BaseURL = server.URL + "/v1"
			m := NewHTTPModel(s)
			defer m.Close()
			_, err := m.Complete(context.Background(), nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "do-not-expose") {
				t.Fatalf("bad error: %v", err)
			}
		})
	}
}

func TestHTTPModelDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer server.Close()
	s := config.DefaultModelSettings()
	s.BaseURL = server.URL
	s.APIKey = "secret"
	m := NewHTTPModel(s)
	defer m.Close()
	_, err := m.Complete(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") || followed.Load() {
		t.Fatal("redirect was followed")
	}
}

func TestHTTPDeadlineCoversSlowResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
				_, _ = w.Write([]byte(" "))
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer server.Close()
	s := config.DefaultModelSettings()
	s.BaseURL = server.URL
	s.Timeout = 40 * time.Millisecond
	m := NewHTTPModel(s)
	defer m.Close()
	started := time.Now()
	_, err := m.Complete(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(started) > time.Second {
		t.Fatalf("deadline failed: %v", err)
	}
}

func TestHTTPInvalidURLDoesNotAttemptRequest(t *testing.T) {
	for _, endpoint := range []string{"file:///etc/passwd", "https://user:password@model/v1", "https://model/v1?token=x"} {
		s := config.DefaultModelSettings()
		s.BaseURL = endpoint
		m := NewHTTPModel(s)
		_, err := m.Complete(context.Background(), nil, nil)
		m.Close()
		if err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
}
