package checkoutsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"token-bench/task/internal/jsoncompare"
)

type paymentRow struct {
	OrderID   string
	ContactID string
	Email     string
	Amount    string
	Currency  string
	Status    string
}

// checkoutSpec describes one valid checkout request and the values it must produce.
type checkoutSpec struct {
	body      string // raw request body, sent verbatim
	email     string
	firstName string
	lastName  string
	amount    string // canonical decimal the backends must see, for example "19.99"
	currency  string
}

type outcome int

const (
	outcomeCompleted outcome = iota
	outcomeCaptureFailed
	outcomeCreateFailed
)

// scenario is one named check: mock configuration plus a sequence of requests and verifications.
type scenario struct {
	name          string
	seeds         []seedContact
	createStatus  int
	captureStatus int
	preload       []paymentRow
	run           func(e *env) string
}

type env struct {
	c      *checkoutSync
	ctx    context.Context
	client *http.Client
	// last request description, used in diagnostics
	last string
}

func (c *checkoutSync) runScenarios(scenarios []scenario) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, current := range scenarios {
		if diagnosis := c.runScenario(ctx, current); diagnosis != "" {
			return fmt.Sprintf("Check %q failed. Diagnosis: %s", current.name, diagnosis), false
		}
	}
	return "", true
}

func (c *checkoutSync) runScenario(ctx context.Context, current scenario) string {
	if _, err := c.database.ExecContext(ctx, "DELETE FROM "+paymentsTable); err != nil {
		return fmt.Sprintf("reset payments table: %v", err)
	}
	c.mocks.reset(current.seeds, current.createStatus, current.captureStatus)
	for _, row := range current.preload {
		_, err := c.database.ExecContext(ctx,
			"INSERT INTO "+paymentsTable+" (order_id, contact_id, email, amount, currency, status) VALUES (?, ?, ?, ?, ?, ?)",
			row.OrderID, row.ContactID, row.Email, row.Amount, row.Currency, row.Status)
		if err != nil {
			return fmt.Sprintf("preload payment row: %v", err)
		}
	}
	e := &env{c: c, ctx: ctx, client: &http.Client{Timeout: 15 * time.Second}}
	return e.withRequest(current.run(e))
}

func (e *env) withRequest(diagnosis string) string {
	if diagnosis == "" || e.last == "" {
		return diagnosis
	}
	return fmt.Sprintf("%s (last request: %s)", diagnosis, e.last)
}

func (e *env) do(method, path, body string) (int, []byte, string) {
	e.last = fmt.Sprintf("%s %s body=%q", method, path, body)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(e.ctx, method, origin(e.c.applicationPort)+path, reader)
	if err != nil {
		return 0, nil, fmt.Sprintf("construct request: %v", err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := e.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Sprintf("request application: %v", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, fmt.Sprintf("read response: %v", err)
	}
	return response.StatusCode, responseBody, ""
}

func (e *env) rows() (map[string]paymentRow, error) {
	result, err := e.c.database.QueryContext(e.ctx,
		"SELECT order_id, contact_id, email, CAST(amount AS CHAR), currency, status FROM "+paymentsTable)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	rows := map[string]paymentRow{}
	for result.Next() {
		var orderID, contactID, email, amount, currency, status sql.NullString
		if err := result.Scan(&orderID, &contactID, &email, &amount, &currency, &status); err != nil {
			return nil, err
		}
		rows[orderID.String] = paymentRow{orderID.String, contactID.String, email.String, amount.String, currency.String, status.String}
	}
	return rows, result.Err()
}

func sameDecimal(left, right string) bool {
	leftValue, leftOK := new(big.Rat).SetString(left)
	rightValue, rightOK := new(big.Rat).SetString(right)
	return leftOK && rightOK && leftValue.Cmp(rightValue) == 0
}

// expectRejected sends a request that must be rejected with 400 and touch no backend or row.
func (e *env) expectRejected(body string) string {
	status, response, problem := e.do(http.MethodPost, "/checkout", body)
	if problem != "" {
		return problem
	}
	if status != http.StatusBadRequest {
		return fmt.Sprintf("expected HTTP 400 for invalid input, got HTTP %d with body %q", status, response)
	}
	if calls := e.c.mocks.snapshot(""); len(calls) != 0 {
		return fmt.Sprintf("invalid input still called HubSpot or PayPal (%d request(s), first %s %s)", len(calls), calls[0].method, calls[0].path)
	}
	rows, err := e.rows()
	if err != nil {
		return fmt.Sprintf("read payments: %v", err)
	}
	if len(rows) != 0 {
		return "invalid input still wrote to the payments table"
	}
	return ""
}

// expectCheckout sends a valid checkout and verifies the response, every backend request
// and the stored row. It returns the PayPal order id (empty when none was created) and a diagnosis.
func (e *env) expectCheckout(spec checkoutSpec, want outcome) (orderID, contactID, diagnosis string) {
	hubspotBefore := len(e.c.mocks.snapshot(serviceHubSpot))
	paypalBefore := len(e.c.mocks.snapshot(serviceOrders))
	rowsBefore, err := e.rows()
	if err != nil {
		return "", "", fmt.Sprintf("read payments: %v", err)
	}
	existing, hadContact := e.c.mocks.contactByEmail(spec.email)
	contactsBefore := e.c.mocks.contactCount()

	status, response, problem := e.do(http.MethodPost, "/checkout", spec.body)
	if problem != "" {
		return "", "", problem
	}
	hubspotCalls := e.c.mocks.snapshot(serviceHubSpot)[hubspotBefore:]
	paypalCalls := e.c.mocks.snapshot(serviceOrders)[paypalBefore:]

	contactID, diagnosis = e.verifyHubSpot(spec, hubspotCalls, existing, hadContact, contactsBefore)
	if diagnosis != "" {
		return "", "", diagnosis
	}
	orderID, diagnosis = e.verifyPayPal(spec, paypalCalls, hubspotCalls, want)
	if diagnosis != "" {
		return "", "", diagnosis
	}

	wantStatus := http.StatusCreated
	if want != outcomeCompleted {
		wantStatus = http.StatusBadGateway
	}
	if status != wantStatus {
		return "", "", fmt.Sprintf("expected HTTP %d, got HTTP %d with body %q", wantStatus, status, response)
	}
	if want == outcomeCompleted {
		expected, _ := json.Marshal(map[string]string{"contactId": contactID, "orderId": orderID, "status": "COMPLETED"})
		equal, compareErr := jsoncompare.Equal(expected, response)
		if compareErr != nil || !equal {
			return "", "", fmt.Sprintf("expected response %s, got %q (comparison error: %v)", expected, response, compareErr)
		}
	}
	return orderID, contactID, e.verifyRow(spec, want, orderID, contactID, rowsBefore)
}

func (e *env) verifyHubSpot(spec checkoutSpec, calls []observedRequest, existing contact, hadContact bool, contactsBefore int) (string, string) {
	if len(calls) != 2 {
		return "", fmt.Sprintf("expected exactly 2 HubSpot requests (a contact search, then one create or update), got %d: %s", len(calls), describe(calls))
	}
	search := calls[0]
	if search.method != http.MethodPost || search.path != "/crm/v3/objects/contacts/search" {
		return "", fmt.Sprintf("the first HubSpot request must be POST /crm/v3/objects/contacts/search, got %s %s", search.method, search.path)
	}
	if !searchesByEmail(search.body, spec.email) {
		return "", fmt.Sprintf("the HubSpot search did not look the contact up by email %q (body %s)", spec.email, search.body)
	}
	write := calls[1]
	contactID := existing.id
	if hadContact {
		if write.method != http.MethodPatch || write.path != "/crm/v3/objects/contacts/"+existing.id {
			return "", fmt.Sprintf("the contact already exists (id %s): expected PATCH /crm/v3/objects/contacts/%s, got %s %s", existing.id, existing.id, write.method, write.path)
		}
	} else {
		if write.method != http.MethodPost || write.path != "/crm/v3/objects/contacts" {
			return "", fmt.Sprintf("the contact does not exist: expected POST /crm/v3/objects/contacts, got %s %s", write.method, write.path)
		}
	}
	properties, ok := requestProperties(write.body)
	if !ok {
		return "", fmt.Sprintf("HubSpot %s body has no properties object: %s", write.method, write.body)
	}
	for key, want := range map[string]string{"firstname": spec.firstName, "lastname": spec.lastName} {
		if properties[key] != want {
			return "", fmt.Sprintf("HubSpot %s property %s = %q, want %q", write.method, key, properties[key], want)
		}
	}
	if !hadContact {
		if properties["email"] != spec.email {
			return "", fmt.Sprintf("HubSpot create property email = %q, want %q", properties["email"], spec.email)
		}
		created, found := e.c.mocks.contactByEmail(spec.email)
		if !found || e.c.mocks.contactCount() != contactsBefore+1 {
			return "", "expected exactly one new HubSpot contact for the email"
		}
		contactID = created.id
	} else if e.c.mocks.contactCount() != contactsBefore {
		return "", "an existing contact must be updated, not duplicated"
	}
	return contactID, ""
}

func (e *env) verifyPayPal(spec checkoutSpec, calls, hubspotCalls []observedRequest, want outcome) (string, string) {
	wantCalls := 2
	if want == outcomeCreateFailed {
		wantCalls = 1
	}
	if len(calls) != wantCalls {
		return "", fmt.Sprintf("expected %d PayPal order request(s), got %d: %s", wantCalls, len(calls), describe(calls))
	}
	create := calls[0]
	if create.method != http.MethodPost || create.path != "/v2/checkout/orders" {
		return "", fmt.Sprintf("the first PayPal request must be POST /v2/checkout/orders, got %s %s", create.method, create.path)
	}
	if create.seq < hubspotCalls[1].seq {
		return "", "PayPal order was created before the HubSpot contact was created or updated"
	}
	if diagnosis := verifyOrderBody(create.body, spec); diagnosis != "" {
		return "", diagnosis
	}
	if want == outcomeCreateFailed {
		return "", ""
	}
	// The order id is the one the mock created; the capture must target it.
	var orderID string
	if ids := orderIDsFromCapturePath(calls[1].path); len(ids) == 1 {
		orderID = ids[0]
	}
	if calls[1].method != http.MethodPost || orderID == "" {
		return "", fmt.Sprintf("the second PayPal request must be POST /v2/checkout/orders/{id}/capture, got %s %s", calls[1].method, calls[1].path)
	}
	if _, known := e.c.mocks.orderByID(orderID); !known {
		return "", fmt.Sprintf("capture targeted unknown order id %q", orderID)
	}
	return orderID, ""
}

func orderIDsFromCapturePath(path string) []string {
	const prefix, suffix = "/v2/checkout/orders/", "/capture"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return nil
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if id == "" || strings.Contains(id, "/") {
		return nil
	}
	return []string{id}
}

func verifyOrderBody(body []byte, spec checkoutSpec) string {
	input, err := decodeObject(body)
	if err != nil {
		return fmt.Sprintf("PayPal create-order body is not a JSON object: %v", err)
	}
	if intent, _ := input["intent"].(string); intent != "CAPTURE" {
		return fmt.Sprintf("PayPal order intent = %v, want CAPTURE", input["intent"])
	}
	units, _ := input["purchase_units"].([]any)
	if len(units) == 0 {
		return "PayPal create-order body has no purchase_units"
	}
	unit, _ := units[0].(map[string]any)
	amount, _ := unit["amount"].(map[string]any)
	if currency, _ := amount["currency_code"].(string); currency != spec.currency {
		return fmt.Sprintf("PayPal purchase_units[0].amount.currency_code = %v, want %s", amount["currency_code"], spec.currency)
	}
	value, isString := amount["value"].(string)
	if !isString || !sameDecimal(value, spec.amount) {
		return fmt.Sprintf("PayPal purchase_units[0].amount.value = %v, want the string %q", amount["value"], spec.amount)
	}
	return ""
}

func (e *env) verifyRow(spec checkoutSpec, want outcome, orderID, contactID string, rowsBefore map[string]paymentRow) string {
	rows, err := e.rows()
	if err != nil {
		return fmt.Sprintf("read payments: %v", err)
	}
	if want == outcomeCreateFailed {
		if len(rows) != len(rowsBefore) {
			return "no payment row may be stored when the PayPal order could not be created"
		}
		return ""
	}
	if len(rows) != len(rowsBefore)+1 {
		return fmt.Sprintf("expected exactly one new payments row, table went from %d to %d rows", len(rowsBefore), len(rows))
	}
	row, ok := rows[orderID]
	if !ok {
		return fmt.Sprintf("no payments row for PayPal order id %q", orderID)
	}
	wantStatus := "COMPLETED"
	if want == outcomeCaptureFailed {
		wantStatus = "FAILED"
	}
	if row.ContactID != contactID || row.Email != spec.email || row.Currency != spec.currency ||
		row.Status != wantStatus || !sameDecimal(row.Amount, spec.amount) {
		return fmt.Sprintf("payments row = %+v, want contact_id=%s email=%s amount=%s currency=%s status=%s",
			row, contactID, spec.email, spec.amount, spec.currency, wantStatus)
	}
	return ""
}

// expectOrder reads GET /checkout/{orderId} and compares the JSON semantically.
func (e *env) expectOrder(orderID string, expected map[string]any) string {
	status, response, problem := e.do(http.MethodGet, "/checkout/"+orderID, "")
	if problem != "" {
		return problem
	}
	if status != http.StatusOK {
		return fmt.Sprintf("expected HTTP 200 for GET /checkout/%s, got HTTP %d with body %q", orderID, status, response)
	}
	expectedJSON, _ := json.Marshal(expected)
	equal, compareErr := jsoncompare.Equal(expectedJSON, response)
	if compareErr != nil || !equal {
		return fmt.Sprintf("expected response %s, got %q (comparison error: %v)", expectedJSON, response, compareErr)
	}
	return ""
}

func (e *env) expectNotFound(orderID string) string {
	status, response, problem := e.do(http.MethodGet, "/checkout/"+orderID, "")
	if problem != "" {
		return problem
	}
	if status != http.StatusNotFound {
		return fmt.Sprintf("expected HTTP 404 for unknown order %q, got HTTP %d with body %q", orderID, status, response)
	}
	return ""
}

func orderDocument(orderID, contactID string, spec checkoutSpec, status string) map[string]any {
	amount, _ := new(big.Rat).SetString(spec.amount)
	number, _ := amount.Float64()
	return map[string]any{
		"orderId": orderID, "contactId": contactID, "email": spec.email,
		"amount": json.Number(fmt.Sprintf("%g", number)), "currency": spec.currency, "status": status,
	}
}

func searchesByEmail(body []byte, email string) bool {
	input, err := decodeObject(body)
	if err != nil {
		return false
	}
	if query, _ := input["query"].(string); query != "" && strings.EqualFold(query, email) {
		return true
	}
	groups, _ := input["filterGroups"].([]any)
	for _, rawGroup := range groups {
		group, _ := rawGroup.(map[string]any)
		filters, _ := group["filters"].([]any)
		for _, rawFilter := range filters {
			filter, _ := rawFilter.(map[string]any)
			if filter["propertyName"] != "email" {
				continue
			}
			if value, _ := filter["value"].(string); filter["operator"] == "EQ" && strings.EqualFold(value, email) {
				return true
			}
		}
	}
	return false
}

func requestProperties(body []byte) (map[string]string, bool) {
	input, err := decodeObject(body)
	if err != nil {
		return nil, false
	}
	return stringProperties(input["properties"])
}

func describe(calls []observedRequest) string {
	if len(calls) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, call.method+" "+call.path)
	}
	return strings.Join(parts, ", ")
}
