package intscenario

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	raw, _ := io.ReadAll(response.Body)
	return response.Code, string(raw)
}

func TestPricingNeedsTheTokenAndAnswersOnlySelectedFields(t *testing.T) {
	handler := PricingHandler(&Recorder{}, &Flakiness{})
	query := `{"query":"query($id: ID!, $skus: [String!]!) { customer(id: $id) { name tier } prices(skus: $skus) { sku price } }","variables":{"id":"c-100","skus":["A-1","B-7","Z-9"]}}`
	if status, _ := do(t, handler, "POST", "/graphql", query, nil); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	status, body := do(t, handler, "POST", "/graphql", query, map[string]string{"Authorization": "Bearer " + PricingToken})
	if strings.Contains(body, `"id"`) {
		t.Errorf("the $id variable must not select an id field: %s", body)
	}
	if status != 200 || !strings.Contains(body, `"tier":"GOLD"`) || strings.Contains(body, "country") || strings.Contains(body, "Z-9") || !strings.Contains(body, `"price":25.5`) {
		t.Fatalf("answer: %d %s", status, body)
	}
}

func TestFraudMockRejectsTheBadIPAndTheBadKey(t *testing.T) {
	handler := FraudHandler(&Recorder{})
	_, bad := do(t, handler, "GET", "/v1/order/screen?ip="+FraudRejectIP+"&key="+FraudKey, "", nil)
	_, good := do(t, handler, "GET", "/v1/order/screen?ip=198.51.100.7&key="+FraudKey, "", nil)
	_, wrong := do(t, handler, "GET", "/v1/order/screen?ip=198.51.100.7&key=nope", "", nil)
	if !strings.Contains(bad, `"REJECT"`) || !strings.Contains(good, `"APPROVE"`) || !strings.Contains(wrong, "INVALID API KEY") {
		t.Fatalf("%s | %s | %s", bad, good, wrong)
	}
}

func TestAIMocksRateTheNoteNotTheInstruction(t *testing.T) {
	instruction := "Decide whether to expedite: HIGH if the customer needs it urgently, rush, expedite. Notes: "
	if got := OrderPriority(instruction + NormalNote); got != "NORMAL" {
		t.Errorf("normal note rated %s", got)
	}
	if got := OrderPriority(instruction + HighNote); got != "HIGH" {
		t.Errorf("high note rated %s", got)
	}
	if got := ProductDescription("Write a description of Trail Lantern for a catalog"); got != ProductDescriptions["Trail Lantern"] {
		t.Errorf("description %q", got)
	}
}

func TestOpenAIShapedAndOllamaAnswerPlainAndToolRequests(t *testing.T) {
	openai := OpenAIShapedHandler(&Recorder{}, func(string) string { return "HIGH" })
	_, plain := do(t, openai, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	_, tool := do(t, openai, "POST", "/v1/chat/completions", `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{"properties":{"priority":{}}}}}]}`, nil)
	if !strings.Contains(plain, `"content":"HIGH"`) || !strings.Contains(tool, `\"priority\":\"HIGH\"`) {
		t.Fatalf("%s | %s", plain, tool)
	}
	ollama := OllamaHandler(&Recorder{}, func(string) string { return "desc" })
	_, chat := do(t, ollama, "POST", "/api/chat", `{"model":"llama3.2","messages":[{"role":"user","content":"x"}],"stream":false}`, nil)
	var decoded struct {
		Done    bool `json:"done"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(chat), &decoded) != nil || !decoded.Done || decoded.Message.Content != "desc" {
		t.Fatalf("ollama: %s", chat)
	}
}

func TestPagerDutyAndMandrillValidateLikeTheRealAPIs(t *testing.T) {
	pager := PagerDutyHandler(&Recorder{})
	incident := `{"incident":{"type":"incident","title":"Poison","service":{"id":"` + PagerDutyService + `","type":"service_reference"}}}`
	if status, _ := do(t, pager, "POST", "/incidents", incident, nil); status != http.StatusBadRequest {
		t.Errorf("missing From must be 400, got %d", status)
	}
	if status, body := do(t, pager, "POST", "/incidents", incident, map[string]string{"From": "ops@example.com"}); status != http.StatusCreated || !strings.Contains(body, `"status":"triggered"`) {
		t.Errorf("incident: %d %s", status, body)
	}
	mandrill := MandrillHandler(&Recorder{})
	send := `{"key":"` + MandrillKey + `","message":{"to":[{"email":"merch@example.com"}],"subject":"s"}}`
	if status, body := do(t, mandrill, "POST", "/api/1.0/messages/send", send, nil); status != 200 || !strings.Contains(body, `"status":"sent"`) {
		t.Errorf("send: %d %s", status, body)
	}
	if status, _ := do(t, mandrill, "POST", "/api/1.0/messages/send", strings.Replace(send, MandrillKey, "bad", 1), nil); status != http.StatusInternalServerError {
		t.Errorf("a bad key must fail: %d", status)
	}
}

func TestPricingFailsTheFlakyCustomerOnceAndResets(t *testing.T) {
	flakiness := &Flakiness{}
	handler := PricingHandler(&Recorder{}, flakiness)
	auth := map[string]string{"Authorization": "Bearer " + PricingToken}
	query := `{"query":"query($id: ID!) { customer(id: $id) { name } }","variables":{"id":"` + PricingFlaky + `"}}`
	if status, _ := do(t, handler, "POST", "/graphql", query, auth); status != http.StatusServiceUnavailable {
		t.Fatalf("first request: %d", status)
	}
	if status, _ := do(t, handler, "POST", "/graphql", query, auth); status != 200 {
		t.Fatalf("retry: %d", status)
	}
	flakiness.Reset()
	if status, _ := do(t, handler, "POST", "/graphql", query, auth); status != http.StatusServiceUnavailable {
		t.Fatalf("after reset: %d", status)
	}
}
