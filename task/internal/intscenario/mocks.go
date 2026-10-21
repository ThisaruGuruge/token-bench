package intscenario

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recorder keeps the requests one mock received.
type Recorder struct {
	mu       sync.Mutex
	requests []Request
}

var recordedHeaders = []string{"Authorization", "From", "Accept", "Content-Type"}

// Record stores the request and returns its body.
func (r *Recorder) Record(request *http.Request) []byte {
	body, _ := io.ReadAll(request.Body)
	headers := map[string]string{}
	for _, name := range recordedHeaders {
		if value := request.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, Request{Seq: len(r.requests) + 1, Method: request.Method, Path: request.URL.Path,
		Query: request.URL.RawQuery, Headers: headers, Body: string(body)})
	return body
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

func notFound(recorder *Recorder, writer http.ResponseWriter, request *http.Request) {
	recorder.Record(request)
	writeJSON(writer, http.StatusNotFound, map[string]any{"message": "no mock endpoint for " + request.Method + " " + request.URL.Path})
}

// --- pricing: a GraphQL API (customers and prices) ---

// PricingToken is the bearer token the pricing GraphQL mock requires.
const PricingToken = "pricing-token"

// Customers and Prices are the pricing mock's data.
var (
	Customers = map[string]map[string]any{
		"c-100": {"id": "c-100", "name": "Ada Gold", "tier": "GOLD", "country": "GB"},
		"c-200": {"id": "c-200", "name": "Bob Standard", "tier": "STANDARD", "country": "US"},
		"c-300": {"id": "c-300", "name": "Cy Retry", "tier": "STANDARD", "country": "DE"},
	}
	Prices = map[string]float64{"A-1": 10.00, "B-7": 25.50, "C-3": 4.25}
)

var quotedArgument = regexp.MustCompile(`"([^"]+)"`)

// PricingFlaky is the customer whose first pricing request is answered 503.
const PricingFlaky = "c-300"

// Flakiness remembers which customers already had their 503.
type Flakiness struct {
	mu     sync.Mutex
	failed map[string]bool
}

// Reset forgets the failures.
func (f *Flakiness) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = map[string]bool{}
}

func (f *Flakiness) failFirst(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed == nil {
		f.failed = map[string]bool{}
	}
	if id != PricingFlaky || f.failed[id] {
		return false
	}
	f.failed[id] = true
	return true
}

// PricingHandler serves POST /graphql: `customer(id)` and `prices(skus)`, with
// variables, answering only the fields the document selects. The first
// request for PricingFlaky is answered 503, as a flaky upstream would.
func PricingHandler(recorder *Recorder, flakiness *Flakiness) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(writer http.ResponseWriter, request *http.Request) {
		body := recorder.Record(request)
		if request.Header.Get("Authorization") != "Bearer "+PricingToken {
			writeJSON(writer, http.StatusUnauthorized, map[string]any{"errors": []any{map[string]any{"message": "unauthorized"}}})
			return
		}
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.Query) == "" {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"errors": []any{map[string]any{"message": "a query is required"}}})
			return
		}
		if flakiness != nil && flakiness.failFirst(stringVariable(payload, "id")) {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"errors": []any{map[string]any{"message": "service unavailable"}}})
			return
		}
		data := map[string]any{}
		if strings.Contains(payload.Query, "customer") {
			id := stringVariable(payload, "id")
			if id == "" {
				if match := quotedArgument.FindStringSubmatch(payload.Query); match != nil {
					id = match[1]
				}
			}
			if customer, ok := Customers[id]; ok {
				data["customer"] = selected(customer, selection(payload.Query, "customer"), "name", "tier", "country", "id")
			} else {
				data["customer"] = nil
			}
		}
		if strings.Contains(payload.Query, "prices") {
			var rows []any
			for _, sku := range stringListVariable(payload, "skus") {
				if price, ok := Prices[sku]; ok {
					rows = append(rows, selected(map[string]any{"sku": sku, "price": price}, selection(payload.Query, "prices"), "sku", "price"))
				}
			}
			data["prices"] = rows
		}
		writeJSON(writer, http.StatusOK, map[string]any{"data": data})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { notFound(recorder, writer, request) })
	return mux
}

func stringVariable(payload struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}, name string) string {
	text, _ := payload.Variables[name].(string)
	return text
}

func stringListVariable(payload struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}, name string) []string {
	list, _ := payload.Variables[name].([]any)
	var out []string
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// selection returns the text of root's own selection set: `root(args) { ... }`.
func selection(query, root string) string {
	match := regexp.MustCompile(`\b` + root + `\s*(\([^)]*\))?\s*\{([^}]*)\}`).FindStringSubmatch(query)
	if match == nil {
		return ""
	}
	return match[2]
}

// selected keeps the fields the document names (a field not mentioned is not
// answered, as a real GraphQL server would).
func selected(row map[string]any, query string, fields ...string) map[string]any {
	out := map[string]any{}
	for _, field := range fields {
		if value, ok := row[field]; ok && regexp.MustCompile(`\b`+field+`\b`).MatchString(query) {
			out[field] = value
		}
	}
	return out
}

// --- fraud: FraudLabs Pro order screening ---

// FraudKey is the licence key the fraud mock accepts.
const FraudKey = "fraud-test-key"

// FraudRejectIP is screened as REJECT; every other address is APPROVE.
const FraudRejectIP = "203.0.113.66"

// FraudHandler serves GET /v1/order/screen as the real API does: the answer is
// a JSON document in the body.
func FraudHandler(recorder *Recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/order/screen", func(writer http.ResponseWriter, request *http.Request) {
		recorder.Record(request)
		query := request.URL.Query()
		if query.Get("key") != FraudKey {
			writeJSON(writer, http.StatusOK, map[string]any{"error": map[string]any{"error_code": "400", "error_message": "INVALID API KEY"}})
			return
		}
		status, score := "APPROVE", 12
		if query.Get("ip") == FraudRejectIP {
			status, score = "REJECT", 98
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"fraudlabspro_id": "20261022-FLP001", "ip_address": query.Get("ip"), "ip_country": "US",
			"fraudlabspro_score": score, "fraudlabspro_distribution": score, "fraudlabspro_status": status,
			"fraudlabspro_credits": 499,
		})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { notFound(recorder, writer, request) })
	return mux
}

// --- AI providers ---

// ChatAnswer decides a chat model's answer from the whole request text.
type ChatAnswer func(requestText string) string

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatTool struct {
	Function struct {
		Name       string `json:"name"`
		Parameters struct {
			Properties map[string]any `json:"properties"`
		} `json:"parameters"`
	} `json:"function"`
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

func firstProperty(tool chatTool) string {
	names := make([]string, 0, len(tool.Function.Parameters.Properties))
	for name := range tool.Function.Parameters.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "result"
	}
	return names[0]
}

// OpenAIShapedHandler serves POST /v1/chat/completions (OpenAI, Mistral and
// every compatible provider). A request offering tools is answered by a call
// of the first tool carrying the answer.
func OpenAIShapedHandler(recorder *Recorder, answer ChatAnswer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		body := recorder.Record(request)
		var payload struct {
			Model    string        `json:"model"`
			Messages []chatMessage `json:"messages"`
			Tools    []chatTool    `json:"tools"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"}})
			return
		}
		text := answer(string(body))
		message := map[string]any{"role": "assistant", "content": text}
		finish := "stop"
		if len(payload.Tools) > 0 {
			arguments, _ := json.Marshal(map[string]any{firstProperty(payload.Tools[0]): text})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function", "function": map[string]any{"name": payload.Tools[0].Function.Name, "arguments": string(arguments)}}}}
			finish = "tool_calls"
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"id": "chatcmpl-bench", "object": "chat.completion", "created": time.Now().Unix(), "model": payload.Model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 20, "completion_tokens": 2, "total_tokens": 22},
		})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { notFound(recorder, writer, request) })
	return mux
}

// Order notes the order-sync grader sends; the AI mock stands in for a perfect
// reader of them whatever instruction wraps them.
const (
	HighNote   = "Please expedite: customer event on Friday."
	NormalNote = "Leave at the front door."
)

// OrderPriority is the order-sync AI mock's answer.
func OrderPriority(requestText string) string {
	if strings.Contains(requestText, HighNote) {
		return "HIGH"
	}
	return "NORMAL"
}

// ProductDescriptions are what the Ollama mock "writes" for the graded products.
var ProductDescriptions = map[string]string{
	"Trail Lantern":  "A rugged LED lantern built for long nights on the trail.",
	"Summit Bottle":  "An insulated steel bottle that keeps drinks cold on the summit.",
	"Ridge Backpack": "A light, weatherproof backpack for day hikes along the ridge.",
}

// ProductDescription is the graph-service AI mock's answer.
func ProductDescription(requestText string) string {
	for name, description := range ProductDescriptions {
		if strings.Contains(requestText, name) {
			return description
		}
	}
	return "A well-made product."
}

// OllamaHandler serves POST /api/chat, Ollama's own protocol.
func OllamaHandler(recorder *Recorder, answer ChatAnswer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", func(writer http.ResponseWriter, request *http.Request) {
		body := recorder.Record(request)
		var payload struct {
			Model string     `json:"model"`
			Tools []chatTool `json:"tools"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		text := answer(string(body))
		message := map[string]any{"role": "assistant", "content": text}
		if len(payload.Tools) > 0 {
			message = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
				"function": map[string]any{"name": payload.Tools[0].Function.Name, "arguments": map[string]any{firstProperty(payload.Tools[0]): text}}}}}
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"model": payload.Model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "message": message,
			"done": true, "done_reason": "stop", "total_duration": 1000000, "eval_count": 8,
		})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { notFound(recorder, writer, request) })
	return mux
}

// --- customers: enrichment for event-bridge ---

// Segments are the customer segments the enrichment mock knows.
var Segments = map[string]string{"acme": "enterprise", "globex": "enterprise", "initech": "smb"}

// CustomersHandler serves GET /customers/{id}.
func CustomersHandler(recorder *Recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /customers/{id}", func(writer http.ResponseWriter, request *http.Request) {
		recorder.Record(request)
		id := request.PathValue("id")
		if segment, ok := Segments[id]; ok {
			writeJSON(writer, http.StatusOK, map[string]any{"id": id, "segment": segment})
			return
		}
		writeJSON(writer, http.StatusNotFound, map[string]any{"message": "unknown customer"})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { notFound(recorder, writer, request) })
	return mux
}

// --- pagerduty: incidents ---

// PagerDutyService is the service id incidents must reference.
const PagerDutyService = "PSVC001"

// PagerDutyHandler serves POST /incidents; like the real API it insists on the
// From header and the incident body's type and service.
func PagerDutyHandler(recorder *Recorder) http.Handler {
	mux := http.NewServeMux()
	var mu sync.Mutex
	count := 0
	mux.HandleFunc("POST /incidents", func(writer http.ResponseWriter, request *http.Request) {
		body := recorder.Record(request)
		if request.Header.Get("From") == "" {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "Invalid Input Provided", "code": 2001, "errors": []string{"From header is required"}}})
			return
		}
		var payload struct {
			Incident struct {
				Type    string `json:"type"`
				Title   string `json:"title"`
				Service struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"service"`
			} `json:"incident"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Incident.Type != "incident" || payload.Incident.Title == "" || payload.Incident.Service.ID == "" {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "Invalid Input Provided", "code": 2001, "errors": []string{"incident type, title and service are required"}}})
			return
		}
		mu.Lock()
		count++
		number := count
		mu.Unlock()
		writeJSON(writer, http.StatusCreated, map[string]any{"incident": map[string]any{
			"id": "PINC" + string(rune('A'+number%26)), "incident_number": number, "title": payload.Incident.Title, "status": "triggered",
			"urgency": "high", "created_at": time.Now().UTC().Format(time.RFC3339),
			"service": map[string]any{"id": payload.Incident.Service.ID, "type": "service_reference"},
		}})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { notFound(recorder, writer, request) })
	return mux
}

// --- mandrill: transactional email ---

// MandrillKey is the API key the mandrill mock accepts, in the request body.
const MandrillKey = "mandrill-test-key"

// MandrillHandler serves POST /api/1.0/messages/send.
func MandrillHandler(recorder *Recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/1.0/messages/send", func(writer http.ResponseWriter, request *http.Request) {
		body := recorder.Record(request)
		var payload struct {
			Key     string `json:"key"`
			Message struct {
				To []struct {
					Email string `json:"email"`
				} `json:"to"`
			} `json:"message"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Key != MandrillKey {
			writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "code": -1, "name": "Invalid_Key", "message": "Invalid API key"})
			return
		}
		var rows []any
		for index, recipient := range payload.Message.To {
			rows = append(rows, map[string]any{"email": recipient.Email, "status": "sent", "_id": "msg" + string(rune('a'+index)), "reject_reason": nil})
		}
		writeJSON(writer, http.StatusOK, rows)
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { notFound(recorder, writer, request) })
	return mux
}
