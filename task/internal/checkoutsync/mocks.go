package checkoutsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	serviceHubSpot = "hubspot"
	serviceOrders  = "paypal"
	serviceAuth    = "paypal-auth"
)

var (
	moneyValuePattern   = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,2})?$`)
	currencyCodePattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

type observedRequest struct {
	seq     int
	service string
	method  string
	path    string
	body    []byte
}

type seedContact struct {
	ID        string
	Email     string
	FirstName string
	LastName  string
}

type contact struct {
	id         string
	properties map[string]string
	createdAt  string
	updatedAt  string
}

type paypalOrder struct {
	id        string
	status    string
	value     string
	currency  string
	captureID string
	createdAt string
	updatedAt string
}

// mocks owns the benchmark's HubSpot and PayPal backends. Both servers share one
// recorder so cross-service ordering can be verified.
type mocks struct {
	hubspotServer *http.Server
	paypalServer  *http.Server

	mu            sync.Mutex
	seq           int
	requests      []observedRequest
	contacts      map[string]*contact
	nextContactID int
	orders        map[string]*paypalOrder
	createStatus  int
	captureStatus int
}

func startMocks(hubspotPort, paypalPort int) (*mocks, error) {
	m := &mocks{}
	m.reset(nil, 0, 0)
	hubspotListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hubspotPort))
	if err != nil {
		return nil, fmt.Errorf("listen for HubSpot mock: %w", err)
	}
	paypalListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", paypalPort))
	if err != nil {
		_ = hubspotListener.Close()
		return nil, fmt.Errorf("listen for PayPal mock: %w", err)
	}
	m.hubspotServer = &http.Server{Handler: trimTrailingSlash(m.hubspotHandler()), ReadHeaderTimeout: 5 * time.Second}
	m.paypalServer = &http.Server{Handler: trimTrailingSlash(m.paypalHandler()), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = m.hubspotServer.Serve(hubspotListener) }()
	go func() { _ = m.paypalServer.Serve(paypalListener) }()
	return m, nil
}

// trimTrailingSlash lets "/path/" reach the "/path" route. The real APIs accept both and the
// Ballerina connectors do send the trailing slash for collection routes such as contact creation.
func trimTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if path := request.URL.Path; len(path) > 1 && strings.HasSuffix(path, "/") {
			trimmed := *request
			trimmedURL := *request.URL
			trimmedURL.Path = strings.TrimRight(path, "/")
			trimmedURL.RawPath = ""
			trimmed.URL = &trimmedURL
			request = &trimmed
		}
		next.ServeHTTP(writer, request)
	})
}

func (m *mocks) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var failures []string
	if err := m.hubspotServer.Shutdown(ctx); err != nil {
		failures = append(failures, "stop HubSpot mock: "+err.Error())
	}
	if err := m.paypalServer.Shutdown(ctx); err != nil {
		failures = append(failures, "stop PayPal mock: "+err.Error())
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

// reset clears recorded requests and orders, seeds contacts, and configures PayPal
// failure injection. A non-zero status makes the matching PayPal operation fail
// with that status.
func (m *mocks) reset(seeds []seedContact, createStatus, captureStatus int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq = 0
	m.requests = nil
	m.contacts = map[string]*contact{}
	m.nextContactID = 1001
	m.orders = map[string]*paypalOrder{}
	m.createStatus = createStatus
	m.captureStatus = captureStatus
	now := timestamp()
	for _, seed := range seeds {
		m.contacts[seed.ID] = &contact{
			id: seed.ID, createdAt: now, updatedAt: now,
			properties: map[string]string{"email": seed.Email, "firstname": seed.FirstName, "lastname": seed.LastName},
		}
	}
}

func (m *mocks) snapshot(service string) []observedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []observedRequest
	for _, request := range m.requests {
		if service == "" || request.service == service {
			result = append(result, request)
		}
	}
	return result
}

func (m *mocks) contactByEmail(email string) (contact, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, current := range m.contacts {
		if strings.EqualFold(current.properties["email"], email) {
			return m.copyContact(current), true
		}
	}
	return contact{}, false
}

func (m *mocks) contactCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.contacts)
}

func (m *mocks) orderByID(id string) (paypalOrder, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.orders[id]
	if !ok {
		return paypalOrder{}, false
	}
	return *current, true
}

func (m *mocks) copyContact(source *contact) contact {
	properties := make(map[string]string, len(source.properties))
	for key, value := range source.properties {
		properties[key] = value
	}
	return contact{id: source.id, properties: properties, createdAt: source.createdAt, updatedAt: source.updatedAt}
}

// record stores the request and returns its body. Callers hold no lock.
func (m *mocks) record(service string, request *http.Request) []byte {
	body, _ := io.ReadAll(request.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.requests = append(m.requests, observedRequest{
		seq: m.seq, service: service, method: request.Method, path: request.URL.Path, body: append([]byte(nil), body...),
	})
	return body
}

func timestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func decodeObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return value, nil
}

// --- HubSpot CRM v3 contacts ---

func hubspotError(writer http.ResponseWriter, status int, category, message string) {
	writeJSON(writer, status, map[string]any{
		"status": "error", "message": message, "correlationId": "6b0f2f0e-1a42-4c9c-9d57-0c1b6a7d5e11", "category": category,
	})
}

func (m *mocks) hubspotHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /crm/v3/objects/contacts/search", m.searchContacts)
	mux.HandleFunc("POST /crm/v3/objects/contacts", m.createContact)
	mux.HandleFunc("GET /crm/v3/objects/contacts/{id}", m.getContact)
	mux.HandleFunc("PATCH /crm/v3/objects/contacts/{id}", m.updateContact)
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		m.record(serviceHubSpot, request)
		hubspotError(writer, http.StatusNotFound, "ENDPOINT_NOT_FOUND", "No HubSpot mock endpoint for "+request.Method+" "+request.URL.Path)
	})
	return mux
}

// contactJSON renders a SimplePublicObject: id, properties, createdAt, updatedAt and archived.
func contactJSON(current contact) map[string]any {
	properties := map[string]string{}
	for key, value := range current.properties {
		properties[key] = value
	}
	properties["hs_object_id"] = current.id
	properties["createdate"] = current.createdAt
	properties["lastmodifieddate"] = current.updatedAt
	return map[string]any{
		"id": current.id, "properties": properties,
		"createdAt": current.createdAt, "updatedAt": current.updatedAt, "archived": false,
	}
}

func stringProperties(raw any) (map[string]string, bool) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	result := make(map[string]string, len(object))
	for key, value := range object {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		result[key] = text
	}
	return result, true
}

func (m *mocks) searchContacts(writer http.ResponseWriter, request *http.Request) {
	body := m.record(serviceHubSpot, request)
	input, err := decodeObject(body)
	if err != nil {
		hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid input JSON on line 1: "+err.Error())
		return
	}
	query, _ := input["query"].(string)
	var groups [][]map[string]any
	if rawGroups, present := input["filterGroups"]; present && rawGroups != nil {
		list, ok := rawGroups.([]any)
		if !ok {
			hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "filterGroups must be an array")
			return
		}
		for _, rawGroup := range list {
			group, ok := rawGroup.(map[string]any)
			filters, filtersOK := group["filters"].([]any)
			if !ok || !filtersOK {
				hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "each filter group requires a filters array")
				return
			}
			var parsed []map[string]any
			for _, rawFilter := range filters {
				filter, ok := rawFilter.(map[string]any)
				if !ok {
					hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "each filter must be an object")
					return
				}
				parsed = append(parsed, filter)
			}
			groups = append(groups, parsed)
		}
	}
	limit := 10
	if rawLimit, present := input["limit"]; present && rawLimit != nil {
		number, ok := rawLimit.(float64)
		if !ok || number < 1 || number > 200 {
			hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "limit must be between 1 and 200")
			return
		}
		limit = int(number)
	}

	m.mu.Lock()
	var matches []contact
	var searchErr string
	for _, current := range m.contacts {
		if query != "" && !contactContains(current, query) {
			continue
		}
		matched := len(groups) == 0
		for _, group := range groups {
			groupMatched := true
			for _, filter := range group {
				ok, problem := filterMatches(current, filter)
				if problem != "" {
					searchErr = problem
				}
				groupMatched = groupMatched && ok
			}
			if groupMatched {
				matched = true
			}
		}
		if matched {
			matches = append(matches, m.copyContact(current))
		}
	}
	m.mu.Unlock()
	if searchErr != "" {
		hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", searchErr)
		return
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].id < matches[j].id })
	total := len(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}
	results := make([]map[string]any, 0, len(matches))
	for _, current := range matches {
		results = append(results, contactJSON(current))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"total": total, "results": results})
}

func contactContains(current *contact, query string) bool {
	for _, value := range current.properties {
		if strings.Contains(strings.ToLower(value), strings.ToLower(query)) {
			return true
		}
	}
	return false
}

// filterMatches evaluates one CRM search filter. Email comparisons are case-insensitive, as in HubSpot.
func filterMatches(current *contact, filter map[string]any) (bool, string) {
	name, _ := filter["propertyName"].(string)
	operator, _ := filter["operator"].(string)
	if name == "" || operator == "" {
		return false, "filters require propertyName and operator"
	}
	value, hasValue := filter["value"].(string)
	actual, present := current.properties[name]
	equal := func(a, b string) bool {
		if name == "email" {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	switch operator {
	case "EQ":
		if !hasValue {
			return false, "EQ filters require a string value"
		}
		return present && equal(actual, value), ""
	case "NEQ":
		return !present || !equal(actual, value), ""
	case "HAS_PROPERTY":
		return present && actual != "", ""
	case "NOT_HAS_PROPERTY":
		return !present || actual == "", ""
	case "CONTAINS_TOKEN":
		return present && strings.Contains(strings.ToLower(actual), strings.ToLower(value)), ""
	case "IN":
		values, _ := filter["values"].([]any)
		for _, candidate := range values {
			if text, ok := candidate.(string); ok && present && equal(actual, text) {
				return true, ""
			}
		}
		return false, ""
	default:
		return false, "unsupported filter operator " + operator
	}
}

func (m *mocks) createContact(writer http.ResponseWriter, request *http.Request) {
	body := m.record(serviceHubSpot, request)
	input, err := decodeObject(body)
	if err != nil {
		hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid input JSON on line 1: "+err.Error())
		return
	}
	properties, ok := stringProperties(input["properties"])
	if !ok {
		hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "properties must be an object of string values")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if email, hasEmail := properties["email"]; hasEmail {
		for _, existing := range m.contacts {
			if strings.EqualFold(existing.properties["email"], email) {
				hubspotError(writer, http.StatusConflict, "CONFLICT", "Contact already exists. Existing ID: "+existing.id)
				return
			}
		}
	}
	now := timestamp()
	created := &contact{id: strconv.Itoa(m.nextContactID), properties: properties, createdAt: now, updatedAt: now}
	m.nextContactID++
	m.contacts[created.id] = created
	writeJSON(writer, http.StatusCreated, contactJSON(m.copyContact(created)))
}

func (m *mocks) getContact(writer http.ResponseWriter, request *http.Request) {
	m.record(serviceHubSpot, request)
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.contacts[request.PathValue("id")]
	if !ok {
		hubspotError(writer, http.StatusNotFound, "OBJECT_NOT_FOUND", "resource not found")
		return
	}
	writeJSON(writer, http.StatusOK, contactJSON(m.copyContact(current)))
}

func (m *mocks) updateContact(writer http.ResponseWriter, request *http.Request) {
	body := m.record(serviceHubSpot, request)
	input, err := decodeObject(body)
	if err != nil {
		hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid input JSON on line 1: "+err.Error())
		return
	}
	properties, ok := stringProperties(input["properties"])
	if !ok {
		hubspotError(writer, http.StatusBadRequest, "VALIDATION_ERROR", "properties must be an object of string values")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.contacts[request.PathValue("id")]
	if !exists {
		hubspotError(writer, http.StatusNotFound, "OBJECT_NOT_FOUND", "resource not found")
		return
	}
	for key, value := range properties {
		current.properties[key] = value
	}
	current.updatedAt = timestamp()
	writeJSON(writer, http.StatusOK, contactJSON(m.copyContact(current)))
}

// --- PayPal OAuth2 and Orders v2 ---

func paypalError(writer http.ResponseWriter, status int, name, message, issue string) {
	writeJSON(writer, status, map[string]any{
		"name": name, "message": message, "debug_id": "b3f1c0a9d2e47",
		"details": []map[string]any{{"issue": issue, "description": message}},
		"links":   []any{},
	})
}

func paypalFailure(writer http.ResponseWriter, status int) {
	switch status {
	case http.StatusBadRequest:
		paypalError(writer, status, "INVALID_REQUEST", "Request is not well-formed, syntactically incorrect, or violates schema.", "MALFORMED_REQUEST_JSON")
	case http.StatusUnauthorized:
		paypalError(writer, status, "AUTHENTICATION_FAILURE", "Authentication failed due to invalid authentication credentials or a missing Authorization header.", "AUTHENTICATION_FAILURE")
	case http.StatusUnprocessableEntity:
		paypalError(writer, status, "UNPROCESSABLE_ENTITY", "The requested action could not be performed, semantically incorrect, or failed business validation.", "INSTRUMENT_DECLINED")
	default:
		paypalError(writer, status, "INTERNAL_SERVER_ERROR", "An internal server error has occurred.", "INTERNAL_SERVER_ERROR")
	}
}

func (m *mocks) paypalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth2/token", func(writer http.ResponseWriter, request *http.Request) {
		m.record(serviceAuth, request)
		writeJSON(writer, http.StatusOK, map[string]any{
			"scope": "https://uri.paypal.com/services/payments/payment", "access_token": "mock-paypal-access-token",
			"token_type": "Bearer", "app_id": "APP-MOCK0000000000", "expires_in": 32400, "nonce": "2026-01-01T00:00:00Zmock",
		})
	})
	mux.HandleFunc("POST /v2/checkout/orders", m.createOrder)
	mux.HandleFunc("GET /v2/checkout/orders/{id}", m.getOrder)
	mux.HandleFunc("POST /v2/checkout/orders/{id}/capture", m.captureOrder)
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		m.record(serviceOrders, request)
		paypalError(writer, http.StatusNotFound, "RESOURCE_NOT_FOUND", "No PayPal mock endpoint for "+request.Method+" "+request.URL.Path, "INVALID_RESOURCE_ID")
	})
	return mux
}

func orderLinks(id string, status string) []map[string]any {
	base := "https://api.sandbox.paypal.com/v2/checkout/orders/" + id
	links := []map[string]any{
		{"href": base, "rel": "self", "method": "GET"},
		{"href": "https://www.sandbox.paypal.com/checkoutnow?token=" + id, "rel": "approve", "method": "GET"},
		{"href": base, "rel": "update", "method": "PATCH"},
		{"href": base + "/capture", "rel": "capture", "method": "POST"},
	}
	if status == "COMPLETED" {
		return links[:1]
	}
	return links
}

func orderJSON(current paypalOrder) map[string]any {
	unit := map[string]any{
		"reference_id": "default",
		"amount":       map[string]any{"currency_code": current.currency, "value": current.value},
		"payee":        map[string]any{"email_address": "merchant@example.com", "merchant_id": "XYZ2345678ABC"},
	}
	result := map[string]any{
		"id": current.id, "status": current.status, "intent": "CAPTURE",
		"create_time": current.createdAt, "update_time": current.updatedAt,
		"purchase_units": []any{unit}, "links": orderLinks(current.id, current.status),
	}
	if current.status == "COMPLETED" {
		unit["payments"] = map[string]any{"captures": []any{map[string]any{
			"id": current.captureID, "status": "COMPLETED",
			"amount":        map[string]any{"currency_code": current.currency, "value": current.value},
			"final_capture": true, "create_time": current.updatedAt, "update_time": current.updatedAt,
			"links": []any{map[string]any{"href": "https://api.sandbox.paypal.com/v2/payments/captures/" + current.captureID, "rel": "self", "method": "GET"}},
		}}}
		result["payer"] = map[string]any{
			"name":          map[string]any{"given_name": "Mock", "surname": "Buyer"},
			"email_address": "buyer@example.com", "payer_id": "PAYER23456789",
		}
	}
	return result
}

func randomSuffix(length int) string {
	const alphabet = "0123456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	result := make([]byte, length)
	for index := range result {
		result[index] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(result)
}

func (m *mocks) createOrder(writer http.ResponseWriter, request *http.Request) {
	body := m.record(serviceOrders, request)
	m.mu.Lock()
	injected := m.createStatus
	m.mu.Unlock()
	if injected != 0 {
		paypalFailure(writer, injected)
		return
	}
	input, err := decodeObject(body)
	if err != nil {
		paypalFailure(writer, http.StatusBadRequest)
		return
	}
	if intent, _ := input["intent"].(string); intent != "CAPTURE" {
		paypalError(writer, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "intent must be CAPTURE", "INVALID_PARAMETER_VALUE")
		return
	}
	units, _ := input["purchase_units"].([]any)
	if len(units) == 0 {
		paypalError(writer, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "purchase_units must contain at least one unit", "INVALID_ARRAY_MIN_ITEMS")
		return
	}
	unit, _ := units[0].(map[string]any)
	amount, _ := unit["amount"].(map[string]any)
	currency, _ := amount["currency_code"].(string)
	value, valueIsString := amount["value"].(string)
	if !currencyCodePattern.MatchString(currency) {
		paypalError(writer, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "amount.currency_code must be a three-letter upper-case code", "CURRENCY_NOT_SUPPORTED")
		return
	}
	if !valueIsString || !moneyValuePattern.MatchString(value) {
		paypalError(writer, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "amount.value must be a string with at most two decimal places", "DECIMAL_PRECISION")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := timestamp()
	created := &paypalOrder{
		id: randomSuffix(17), status: "CREATED", value: value, currency: currency, createdAt: now, updatedAt: now,
	}
	m.orders[created.id] = created
	writeJSON(writer, http.StatusCreated, orderJSON(*created))
}

func (m *mocks) getOrder(writer http.ResponseWriter, request *http.Request) {
	m.record(serviceOrders, request)
	current, ok := m.orderByID(request.PathValue("id"))
	if !ok {
		paypalError(writer, http.StatusNotFound, "RESOURCE_NOT_FOUND", "The specified resource does not exist.", "INVALID_RESOURCE_ID")
		return
	}
	writeJSON(writer, http.StatusOK, orderJSON(current))
}

func (m *mocks) captureOrder(writer http.ResponseWriter, request *http.Request) {
	m.record(serviceOrders, request)
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.orders[request.PathValue("id")]
	if !ok {
		paypalError(writer, http.StatusNotFound, "RESOURCE_NOT_FOUND", "The specified resource does not exist.", "INVALID_RESOURCE_ID")
		return
	}
	if m.captureStatus != 0 {
		paypalFailure(writer, m.captureStatus)
		return
	}
	if current.status == "COMPLETED" {
		paypalError(writer, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "Order already captured.", "ORDER_ALREADY_CAPTURED")
		return
	}
	current.status = "COMPLETED"
	current.captureID = randomSuffix(17)
	current.updatedAt = timestamp()
	writeJSON(writer, http.StatusCreated, orderJSON(*current))
}
