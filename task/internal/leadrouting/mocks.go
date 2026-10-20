// Package leadrouting is the integration scenario of the grammar benchmark: an
// HTTP service that enriches a lead over HTTP, classifies it with an AI model,
// upserts it in HubSpot, publishes it to Kafka, and a Kafka consumer that
// mails hot leads through Gmail. Every external API is a mock sidecar on a
// per-run docker network; Kafka is a real broker.
package leadrouting

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Request is one request a mock received.
type Request struct {
	Seq    int    `json:"seq"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
	// Decoded is a mock-specific reading of the body (Gmail: the MIME message).
	Decoded string `json:"decoded,omitempty"`
}

// Recorder keeps the requests of one mock.
type Recorder struct {
	mu       sync.Mutex
	requests []Request
}

func (r *Recorder) record(request *http.Request) []byte {
	body, _ := io.ReadAll(request.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, Request{Seq: len(r.requests) + 1, Method: request.Method, Path: request.URL.Path, Body: string(body)})
	return body
}

func (r *Recorder) decorate(decoded string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests[len(r.requests)-1].Decoded = decoded
}

// Requests returns a copy of the recorded requests.
func (r *Recorder) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.requests...)
}

// Reset forgets every request.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

// Enrichments is what the company-enrichment API knows; anything else is 404.
var Enrichments = map[string]map[string]any{
	"Acme Corp":   {"company": "Acme Corp", "industry": "Manufacturing", "employees": 1200},
	"Globex":      {"company": "Globex", "industry": "Energy", "employees": 5400},
	"Initech":     {"company": "Initech", "industry": "Software", "employees": 300},
	"Umbrella Co": {"company": "Umbrella Co", "industry": "Pharmaceuticals", "employees": 9000},
}

// EnrichHandler serves GET /companies/{name}.
func EnrichHandler(recorder *Recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /companies/{name}", func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		if company, ok := Enrichments[request.PathValue("name")]; ok {
			writeJSON(writer, http.StatusOK, company)
			return
		}
		writeJSON(writer, http.StatusNotFound, map[string]any{"message": "unknown company"})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writeJSON(writer, http.StatusNotFound, map[string]any{"message": "no enrichment endpoint for " + request.Method + " " + request.URL.Path})
	})
	return mux
}

// HotWords make the AI mock answer HOT; any other message is COLD.
var HotWords = []string{"pricing", "quote", "buy", "purchase", "demo"}

// Rating is the deterministic classification of a message.
func Rating(text string) string {
	lower := strings.ToLower(text)
	for _, word := range HotWords {
		if strings.Contains(lower, word) {
			return "HOT"
		}
	}
	return "COLD"
}

// OpenAIHandler serves POST /v1/chat/completions in OpenAI's shape. The
// answer is HOT or COLD from the last user message; when the request offers
// tools (ballerina/ai's typed generate), it answers with a call of the first
// tool whose arguments carry the rating in the schema's only property.
func OpenAIHandler(recorder *Recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		body := recorder.record(request)
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name       string `json:"name"`
					Parameters struct {
						Properties map[string]any `json:"properties"`
					} `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"}})
			return
		}
		text := ""
		for _, message := range payload.Messages {
			if message.Role == "user" {
				text = contentText(message.Content)
			}
		}
		rating := Rating(text)
		message := map[string]any{"role": "assistant", "content": rating}
		finish := "stop"
		if len(payload.Tools) > 0 {
			tool := payload.Tools[0].Function
			key := "result"
			for name := range tool.Parameters.Properties {
				key = name
				break
			}
			arguments, _ := json.Marshal(map[string]any{key: rating})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "call_lead_1", "type": "function", "function": map[string]any{"name": tool.Name, "arguments": string(arguments)},
			}}}
			finish = "tool_calls"
		}
		model := payload.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"id": "chatcmpl-lead", "object": "chat.completion", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish, "logprobs": nil}},
			"usage":   map[string]any{"prompt_tokens": 20, "completion_tokens": 1, "total_tokens": 21},
		})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "no OpenAI mock endpoint for " + request.Method + " " + request.URL.Path, "type": "invalid_request_error"}})
	})
	return mux
}

func contentText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, part := range typed {
			if object, ok := part.(map[string]any); ok {
				if text, ok := object["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// GmailHandler serves POST /gmail/v1/users/{userId}/messages/send. The raw
// MIME message is decoded into the recorded request.
func GmailHandler(recorder *Recorder) http.Handler {
	mux := http.NewServeMux()
	sent := 0
	var mu sync.Mutex
	mux.HandleFunc("POST /gmail/v1/users/{userId}/messages/send", func(writer http.ResponseWriter, request *http.Request) {
		body := recorder.record(request)
		var payload struct {
			Raw string `json:"raw"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.Raw != "" {
			if decoded, err := decodeBase64(payload.Raw); err == nil {
				recorder.decorate(decoded)
			}
		}
		mu.Lock()
		sent++
		id := "msg-" + strings.Repeat("0", 3) + string(rune('0'+sent%10))
		mu.Unlock()
		writeJSON(writer, http.StatusOK, map[string]any{"id": id, "threadId": "thread-" + id, "labelIds": []string{"SENT"}})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "no Gmail mock endpoint for " + request.Method + " " + request.URL.Path}})
	})
	return mux
}

func decodeBase64(raw string) (string, error) {
	for _, encoding := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := encoding.DecodeString(raw); err == nil {
			return string(decoded), nil
		}
	}
	_, err := base64.StdEncoding.DecodeString(raw)
	return "", err
}
