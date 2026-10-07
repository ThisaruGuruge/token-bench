package checkoutsync

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

func TestPromptIncludesBackendsSchemaAndContract(t *testing.T) {
	prompt, err := New([]int{19080, 19081, 19082, 19083}).Prompt()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"http://127.0.0.1:19081", "http://127.0.0.1:19082", "http://127.0.0.1:19082/v1/oauth2/token",
		"http://127.0.0.1:19081/crm/v3/objects/contacts", "http://127.0.0.1:19082/v2/checkout",
		"Port: `19083`", "Database: `tokenbench`", "Username: `tokenbench`", "Password: `tokenbench`", "Table: `payments`",
		"order_id VARCHAR(64) PRIMARY KEY", "contact_id VARCHAR(64)", "amount DECIMAL(12,2)",
		"ballerinax/hubspot.crm.obj.contacts", "ballerinax/paypal.orders", "ballerinax/mysql",
		"POST /crm/v3/objects/contacts/search", "PATCH /crm/v3/objects/contacts/{id}", "POST /crm/v3/objects/contacts",
		"POST /v2/checkout/orders", "/capture", "HTTP 502", "HTTP 404", "HTTP 201", "`FAILED`", "`COMPLETED`",
		"already running and are owned by the benchmark", "Never stop, kill, replace, reconfigure, or otherwise modify any of these backends",
	} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("prompt does not contain %q", expected)
		}
	}
	if strings.Contains(prompt, "{{") {
		t.Error("prompt contains an unrendered template action")
	}
}

func TestNewRejectsInvalidPorts(t *testing.T) {
	for _, ports := range [][]int{{19080}, {19080, 19081, 19082, 0}, {19080, 19081, 19082, 65536}, {1, 2, 3, 4, 5}} {
		if _, err := New(ports).Prompt(); err == nil {
			t.Errorf("New(%v) did not reject invalid ports", ports)
		}
	}
	if _, err := New([]int{1, 2}).Setup(); err == nil {
		t.Error("Setup accepted invalid ports")
	}
}

func TestCheckHelpers(t *testing.T) {
	if !sameDecimal("12", "12.00") || !sameDecimal("1.2e1", "12.0") || sameDecimal("12.01", "12") || sameDecimal("x", "1") {
		t.Error("sameDecimal misbehaves")
	}
	filter := `{"filterGroups":[{"filters":[{"propertyName":"email","operator":"EQ","value":"A@b.co"}]}]}`
	if !searchesByEmail([]byte(filter), "a@b.co") || searchesByEmail([]byte(filter), "other@b.co") {
		t.Error("searchesByEmail EQ filter")
	}
	if !searchesByEmail([]byte(`{"query":"a@b.co"}`), "a@b.co") || searchesByEmail([]byte(`{}`), "a@b.co") || searchesByEmail([]byte(`x`), "a@b.co") {
		t.Error("searchesByEmail query/empty/invalid")
	}
	if _, ok := requestProperties([]byte(`{"properties":{"a":1}}`)); ok {
		t.Error("non-string property accepted")
	}
	if properties, ok := requestProperties([]byte(`{"properties":{"a":"b"}}`)); !ok || properties["a"] != "b" {
		t.Error("string properties rejected")
	}
	if ids := orderIDsFromCapturePath("/v2/checkout/orders/ABC/capture"); len(ids) != 1 || ids[0] != "ABC" {
		t.Errorf("capture path ids = %v", ids)
	}
	for _, path := range []string{"/v2/checkout/orders//capture", "/v2/checkout/orders/A/B/capture", "/v2/checkout/orders/A"} {
		if ids := orderIDsFromCapturePath(path); len(ids) != 0 {
			t.Errorf("path %q yielded ids %v", path, ids)
		}
	}
}

// --- mock fidelity: shapes follow the HubSpot CRM v3 and PayPal Orders v2 types the Ballerina connectors bind to ---

type mockClient struct {
	t       *testing.T
	hubspot string
	paypal  string
}

func startTestMocks(t *testing.T) (*mocks, mockClient) {
	t.Helper()
	ports := testPorts(t, 2)
	m, err := startMocks(ports[0], ports[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.stop() })
	return m, mockClient{t: t, hubspot: origin(ports[0]), paypal: origin(ports[1])}
}

func (c mockClient) call(method, url, body string) (int, map[string]any) {
	c.t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			c.t.Fatalf("%s %s returned non-JSON %q", method, url, data)
		}
	}
	return response.StatusCode, decoded
}

func requireKeys(t *testing.T, what string, object map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Errorf("%s lacks required field %q: %v", what, key, object)
		}
	}
}

func requireContactShape(t *testing.T, what string, object map[string]any) {
	t.Helper()
	// SimplePublicObject: createdAt, id, properties and updatedAt are non-optional.
	requireKeys(t, what, object, "id", "properties", "createdAt", "updatedAt", "archived")
	if _, ok := object["id"].(string); !ok {
		t.Errorf("%s id is not a string: %v", what, object["id"])
	}
	properties, _ := object["properties"].(map[string]any)
	for key, value := range properties {
		if _, ok := value.(string); !ok {
			t.Errorf("%s property %s is not a string: %v", what, key, value)
		}
	}
}

func TestHubSpotMockFollowsCRMv3(t *testing.T) {
	m, client := startTestMocks(t)
	m.reset([]seedContact{{ID: "501", Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"}}, 0, 0)
	base := client.hubspot + "/crm/v3/objects/contacts"

	status, search := client.call(http.MethodPost, base+"/search", `{"filterGroups":[{"filters":[{"propertyName":"email","operator":"EQ","value":"GRACE@example.com"}]}],"limit":1}`)
	if status != http.StatusOK {
		t.Fatalf("search status %d", status)
	}
	requireKeys(t, "search response", search, "total", "results")
	results := search["results"].([]any)
	if search["total"].(float64) != 1 || len(results) != 1 {
		t.Fatalf("expected the seeded contact, got %v", search)
	}
	requireContactShape(t, "search result", results[0].(map[string]any))

	_, none := client.call(http.MethodPost, base+"/search", `{"filterGroups":[{"filters":[{"propertyName":"email","operator":"EQ","value":"nobody@example.com"}]}]}`)
	if none["total"].(float64) != 0 || len(none["results"].([]any)) != 0 {
		t.Errorf("search for an unknown email returned %v", none)
	}
	if status, _ := client.call(http.MethodPost, base+"/search", `{"filterGroups":[{"filters":[{"propertyName":"email","operator":"BOGUS","value":"x"}]}]}`); status != http.StatusBadRequest {
		t.Errorf("unsupported operator status %d", status)
	}
	if status, _ := client.call(http.MethodPost, base+"/search", `not json`); status != http.StatusBadRequest {
		t.Errorf("malformed search status %d", status)
	}

	status, created := client.call(http.MethodPost, base+"/", `{"properties":{"email":"ada@example.com","firstname":"Ada","lastname":"Lovelace"}}`)
	if status != http.StatusCreated {
		t.Fatalf("create status %d: %v", status, created)
	}
	requireContactShape(t, "created contact", created)
	if created["id"] == "501" {
		t.Error("created contact reused a seeded id")
	}
	if status, conflict := client.call(http.MethodPost, base, `{"properties":{"email":"ADA@example.com"}}`); status != http.StatusConflict {
		t.Errorf("duplicate create status %d: %v", status, conflict)
	}
	if status, _ := client.call(http.MethodPost, base, `{"properties":{"email":1}}`); status != http.StatusBadRequest {
		t.Errorf("non-string property status %d", status)
	}

	status, patched := client.call(http.MethodPatch, base+"/501", `{"properties":{"firstname":"G.","lastname":"M. Hopper"}}`)
	if status != http.StatusOK {
		t.Fatalf("patch status %d", status)
	}
	requireContactShape(t, "patched contact", patched)
	properties := patched["properties"].(map[string]any)
	if properties["firstname"] != "G." || properties["email"] != "grace@example.com" {
		t.Errorf("patch did not merge properties: %v", properties)
	}
	if status, _ := client.call(http.MethodPatch, base+"/999999", `{"properties":{}}`); status != http.StatusNotFound {
		t.Errorf("patch of unknown contact status %d", status)
	}
	if status, _ := client.call(http.MethodGet, base+"/501", ``); status != http.StatusOK {
		t.Errorf("get contact status %d", status)
	}

	calls := m.snapshot(serviceHubSpot)
	if len(calls) != 10 || calls[0].seq != 1 || calls[9].seq != 10 {
		t.Errorf("recorded %d HubSpot requests: %s", len(calls), describe(calls))
	}
}

func TestPayPalMockFollowsOrdersV2(t *testing.T) {
	m, client := startTestMocks(t)
	m.reset(nil, 0, 0)

	status, token := client.call(http.MethodPost, client.paypal+"/v1/oauth2/token", `grant_type=client_credentials`)
	if status != http.StatusOK || token["access_token"] == "" || token["token_type"] != "Bearer" {
		t.Fatalf("token response %d %v", status, token)
	}
	if _, ok := token["expires_in"].(float64); !ok {
		t.Errorf("token lacks numeric expires_in: %v", token)
	}
	if len(m.snapshot(serviceOrders)) != 0 {
		t.Error("token requests must not count as order requests")
	}

	orders := client.paypal + "/v2/checkout/orders"
	status, order := client.call(http.MethodPost, orders, `{"intent":"CAPTURE","purchase_units":[{"amount":{"currency_code":"USD","value":"19.99"}}]}`)
	if status != http.StatusCreated {
		t.Fatalf("create order status %d: %v", status, order)
	}
	requireKeys(t, "order", order, "id", "status", "intent", "purchase_units", "links", "create_time")
	if order["status"] != "CREATED" {
		t.Errorf("new order status %v", order["status"])
	}
	for _, link := range order["links"].([]any) {
		requireKeys(t, "link", link.(map[string]any), "href", "rel", "method")
	}
	unit := order["purchase_units"].([]any)[0].(map[string]any)
	amount := unit["amount"].(map[string]any)
	if amount["value"] != "19.99" || amount["currency_code"] != "USD" {
		t.Errorf("order amount %v", amount)
	}
	id := order["id"].(string)

	status, captured := client.call(http.MethodPost, orders+"/"+id+"/capture", `{}`)
	if status != http.StatusCreated || captured["status"] != "COMPLETED" || captured["id"] != id {
		t.Fatalf("capture response %d %v", status, captured)
	}
	captures := captured["purchase_units"].([]any)[0].(map[string]any)["payments"].(map[string]any)["captures"].([]any)
	if len(captures) != 1 {
		t.Fatalf("expected one capture, got %v", captures)
	}
	capture := captures[0].(map[string]any)
	requireKeys(t, "capture", capture, "id", "status", "amount", "final_capture", "create_time")
	requireKeys(t, "capture amount", capture["amount"].(map[string]any), "value", "currency_code")
	if capture["status"] != "COMPLETED" {
		t.Errorf("capture status %v", capture["status"])
	}
	if status, again := client.call(http.MethodPost, orders+"/"+id+"/capture", `{}`); status != http.StatusUnprocessableEntity {
		t.Errorf("second capture status %d: %v", status, again)
	} else {
		requireKeys(t, "PayPal error", again, "name", "message", "debug_id", "details")
	}
	if status, _ := client.call(http.MethodPost, orders+"/UNKNOWN/capture", `{}`); status != http.StatusNotFound {
		t.Errorf("capture of unknown order status %d", status)
	}
	if status, fetched := client.call(http.MethodGet, orders+"/"+id, ``); status != http.StatusOK || fetched["status"] != "COMPLETED" {
		t.Errorf("get order %d %v", status, fetched)
	}

	for name, body := range map[string]string{
		"numeric value":       `{"intent":"CAPTURE","purchase_units":[{"amount":{"currency_code":"USD","value":19.99}}]}`,
		"three decimals":      `{"intent":"CAPTURE","purchase_units":[{"amount":{"currency_code":"USD","value":"19.999"}}]}`,
		"authorize intent":    `{"intent":"AUTHORIZE","purchase_units":[{"amount":{"currency_code":"USD","value":"1.00"}}]}`,
		"lower-case currency": `{"intent":"CAPTURE","purchase_units":[{"amount":{"currency_code":"usd","value":"1.00"}}]}`,
		"no purchase units":   `{"intent":"CAPTURE","purchase_units":[]}`,
	} {
		if status, _ := client.call(http.MethodPost, orders, body); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", name, status)
		}
	}
	if status, _ := client.call(http.MethodPost, orders, `nope`); status != http.StatusBadRequest {
		t.Errorf("malformed order status %d", status)
	}
}

func TestPayPalMockFailureInjection(t *testing.T) {
	m, client := startTestMocks(t)
	orders := client.paypal + "/v2/checkout/orders"
	create := `{"intent":"CAPTURE","purchase_units":[{"amount":{"currency_code":"EUR","value":"5.00"}}]}`

	m.reset(nil, http.StatusInternalServerError, 0)
	if status, body := client.call(http.MethodPost, orders, create); status != http.StatusInternalServerError {
		t.Errorf("injected create failure status %d", status)
	} else {
		requireKeys(t, "PayPal error", body, "name", "message", "debug_id")
	}

	m.reset(nil, 0, http.StatusUnprocessableEntity)
	_, order := client.call(http.MethodPost, orders, create)
	id := order["id"].(string)
	if status, _ := client.call(http.MethodPost, orders+"/"+id+"/capture", `{}`); status != http.StatusUnprocessableEntity {
		t.Errorf("injected capture failure status %d", status)
	}
	if current, _ := m.orderByID(id); current.status != "CREATED" {
		t.Errorf("failed capture left status %q", current.status)
	}
}

func TestMocksResetClearsState(t *testing.T) {
	m, client := startTestMocks(t)
	m.reset([]seedContact{{ID: "1", Email: "a@example.com"}}, 0, 0)
	client.call(http.MethodPost, client.paypal+"/v2/checkout/orders", `{"intent":"CAPTURE","purchase_units":[{"amount":{"currency_code":"EUR","value":"5.00"}}]}`)
	m.reset(nil, 0, 0)
	if len(m.snapshot("")) != 0 || m.contactCount() != 0 {
		t.Error("reset left requests or contacts")
	}
}

func TestStopFreesPorts(t *testing.T) {
	ports := testPorts(t, 2)
	m, err := startMocks(ports[0], ports[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startMocks(ports[0], ports[1]); err == nil {
		t.Error("starting mocks on busy ports succeeded")
	}
	if err := m.stop(); err != nil {
		t.Fatal(err)
	}
	for _, port := range ports {
		listener, err := net.Listen("tcp", origin(port)[len("http://"):])
		if err != nil {
			t.Errorf("port %d still busy: %v", port, err)
			continue
		}
		_ = listener.Close()
	}
}

func testPorts(t *testing.T, count int) []int {
	t.Helper()
	ports := make([]int, 0, count)
	seen := make(map[int]struct{}, count)
	for len(ports) < count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		if _, duplicate := seen[port]; duplicate {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	return ports
}

func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker integration test in short mode")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker daemon is not available")
	}
}

// --- Docker-backed lifecycle, reference implementation and negative tests ---

func TestReferenceImplementationPassesFeedbackAndValidation(t *testing.T) {
	requireDocker(t)
	ports := testPorts(t, requiredPorts)
	handle := New(ports)
	cleanup, err := handle.Setup()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if _, err := handle.Setup(); err == nil {
		t.Fatal("second Setup unexpectedly succeeded")
	}
	containerName := handle.containerName

	var columns int
	if err := handle.database.QueryRow("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'payments'", databaseName).Scan(&columns); err != nil || columns != 6 {
		t.Fatalf("payments table has %d columns (error %v)", columns, err)
	}

	passing := map[string]variant{
		"strict reference": {},
		"reordered fields, odd numbers, bare decimals": {reorderedResponses: true},
	}
	for name, current := range passing {
		t.Run(name, func(t *testing.T) {
			stop := startReferenceApp(t, handle, current)
			defer stop()
			feedback, err := handle.Feedback()
			if err != nil {
				t.Fatal(err)
			}
			if feedback != "" {
				t.Fatal(feedback)
			}
			passed, err := handle.Validation()
			if err != nil {
				t.Fatal(err)
			}
			if !passed {
				t.Fatal("hidden validation failed")
			}
		})
	}

	failing := map[string]variant{
		"skips the contact search":           {skipSearch: true},
		"uses the first search result":       {pickFirstContact: true},
		"never updates existing contacts":    {createOnlyWhenEmpty: true},
		"forwards the wrong amount":          {wrongAmount: true},
		"forwards the wrong currency":        {wrongCurrency: true},
		"sends the PayPal value as a number": {numberAmountToPayPal: true},
		"stores no payment row":              {noRow: true},
		"stores no FAILED row":               {noFailedRow: true},
		"stores a row without an order":      {rowOnCreateFailure: true},
		"calls backends before validating":   {validateLate: true},
		"captures twice":                     {captureTwice: true},
		"charges before syncing the contact": {paypalFirst: true},
		"returns the amount as a string":     {stringAmountInGet: true},
		"returns 500 instead of 502":         {wrongFailureStatus: true},
		"answers GET from memory, not MySQL": {memoryGet: true},
	}
	for name, current := range failing {
		t.Run("negative: "+name, func(t *testing.T) {
			stop := startReferenceApp(t, handle, current)
			defer stop()
			feedback, err := handle.Feedback()
			if err != nil {
				t.Fatal(err)
			}
			passed, err := handle.Validation()
			if err != nil {
				t.Fatal(err)
			}
			if feedback == "" && passed {
				t.Fatal("wrong implementation passed both feedback and validation")
			}
			if feedback != "" && !strings.Contains(feedback, "Diagnosis") {
				t.Errorf("feedback lacks a diagnosis: %q", feedback)
			}
		})
	}

	t.Run("no application running", func(t *testing.T) {
		feedback, err := handle.Feedback()
		if err != nil || !strings.Contains(feedback, "application health") {
			t.Fatalf("feedback = %q, err = %v", feedback, err)
		}
	})

	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup failed: %v", err)
	}
	if err := exec.Command("docker", "inspect", containerName).Run(); err == nil {
		t.Fatalf("MySQL container %q was not removed", containerName)
	}
}
