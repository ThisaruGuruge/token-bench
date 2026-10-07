package checkoutsync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// variant selects how the reference application deviates from the contract.
type variant struct {
	skipSearch           bool // always create the contact
	pickFirstContact     bool // search without an email filter and use the first result
	wrongAmount          bool // forward amount * 2 to PayPal
	wrongCurrency        bool // forward USD regardless of input
	noRow                bool // never store a payment row
	noFailedRow          bool // skip the FAILED row after a failed capture
	rowOnCreateFailure   bool // store a row even though no order exists
	validateLate         bool // contact HubSpot before validating input
	captureTwice         bool // capture the order two times
	paypalFirst          bool // run PayPal before HubSpot
	stringAmountInGet    bool // GET returns amount as a JSON string
	wrongFailureStatus   bool // 500 instead of 502
	memoryGet            bool // answer GET from memory instead of MySQL
	createOnlyWhenEmpty  bool // never patch an existing contact
	numberAmountToPayPal bool // send the PayPal value as a JSON number
	// Robustness variants that must still pass.
	reorderedResponses bool // different field order, different number notation, bare decimals
}

type referenceApp struct {
	variant  variant
	db       *sql.DB
	hubspot  string
	paypal   string
	mu       sync.Mutex
	memory   map[string]paymentRow
	client   *http.Client
	server   *http.Server
	stopOnce sync.Once
}

var emailPattern = regexp.MustCompile(`^[^@]+@.+$`)

func startReferenceApp(t *testing.T, handle *checkoutSync, v variant) func() {
	t.Helper()
	db, err := sql.Open("mysql", handle.databaseDSN())
	if err != nil {
		t.Fatal(err)
	}
	app := &referenceApp{
		variant: v, db: db, hubspot: origin(handle.hubspotPort), paypal: origin(handle.paypalPort),
		memory: map[string]paymentRow{}, client: &http.Client{Timeout: 10 * time.Second},
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", handle.applicationPort))
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, "ok") })
	mux.HandleFunc("POST /checkout", app.checkout)
	mux.HandleFunc("GET /checkout/{orderId}", app.getCheckout)
	app.server = &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = app.server.Serve(listener) }()
	return func() {
		app.stopOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = app.server.Shutdown(ctx)
			_ = db.Close()
		})
	}
}

type checkoutInput struct {
	email, first, last, currency string
	amount                       *big.Rat
}

func parseInput(body []byte) (checkoutInput, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return checkoutInput{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return checkoutInput{}, false
	}
	text := func(key string) (string, bool) {
		value, ok := raw[key].(string)
		return value, ok && value != ""
	}
	var input checkoutInput
	var ok1, ok2, ok3, ok4 bool
	input.email, ok1 = text("email")
	input.first, ok2 = text("firstName")
	input.last, ok3 = text("lastName")
	input.currency, ok4 = text("currency")
	if !ok1 || !ok2 || !ok3 || !ok4 || !emailPattern.MatchString(input.email) || !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(input.currency) {
		return checkoutInput{}, false
	}
	var literal string
	switch amount := raw["amount"].(type) {
	case json.Number:
		literal = amount.String()
	case string:
		if !regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`).MatchString(amount) {
			return checkoutInput{}, false
		}
		literal = amount
	default:
		return checkoutInput{}, false
	}
	rat, ok := new(big.Rat).SetString(literal)
	if !ok || rat.Sign() <= 0 || new(big.Rat).Mul(rat, big.NewRat(100, 1)).Denom().Cmp(big.NewInt(1)) != 0 {
		return checkoutInput{}, false
	}
	input.amount = rat
	return input, true
}

func (a *referenceApp) post(method, url string, body any) (int, []byte, error) {
	encoded, _ := json.Marshal(body)
	request, err := http.NewRequest(method, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer anything")
	response, err := a.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

func (a *referenceApp) syncContact(input checkoutInput) (string, error) {
	base := a.hubspot + "/crm/v3/objects/contacts"
	var id string
	if !a.variant.skipSearch {
		filters := []any{map[string]any{"propertyName": "email", "operator": "EQ", "value": input.email}}
		if a.variant.pickFirstContact {
			filters = nil
		}
		status, data, err := a.post(http.MethodPost, base+"/search", map[string]any{
			"filterGroups": []any{map[string]any{"filters": filters}}, "limit": 1,
		})
		if err != nil || status != http.StatusOK {
			return "", fmt.Errorf("search: %v %d", err, status)
		}
		var found struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &found); err != nil {
			return "", err
		}
		if len(found.Results) > 0 && !a.variant.createOnlyWhenEmpty {
			id = found.Results[0].ID
		}
	}
	if id != "" {
		status, _, err := a.post(http.MethodPatch, base+"/"+id, map[string]any{"properties": map[string]string{"firstname": input.first, "lastname": input.last}})
		if err != nil || status != http.StatusOK {
			return "", fmt.Errorf("patch: %v %d", err, status)
		}
		return id, nil
	}
	status, data, err := a.post(http.MethodPost, base, map[string]any{"properties": map[string]string{"email": input.email, "firstname": input.first, "lastname": input.last}})
	if err != nil || status != http.StatusCreated {
		return "", fmt.Errorf("create: %v %d", err, status)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (a *referenceApp) createOrder(input checkoutInput) (string, error) {
	amount, currency := input.amount, input.currency
	if a.variant.wrongAmount {
		amount = new(big.Rat).Mul(amount, big.NewRat(2, 1))
	}
	if a.variant.wrongCurrency {
		currency = "USD"
	}
	var value any = amount.FloatString(2)
	if a.variant.reorderedResponses && amount.IsInt() {
		value = amount.FloatString(0)
	}
	if a.variant.numberAmountToPayPal {
		float, _ := amount.Float64()
		value = float
	}
	status, data, err := a.post(http.MethodPost, a.paypal+"/v2/checkout/orders", map[string]any{
		"intent":         "CAPTURE",
		"purchase_units": []any{map[string]any{"amount": map[string]any{"currency_code": currency, "value": value}}},
	})
	if err != nil || status < 200 || status > 299 {
		return "", fmt.Errorf("create order: %v %d", err, status)
	}
	var order struct {
		ID string `json:"id"`
	}
	return order.ID, json.Unmarshal(data, &order)
}

func (a *referenceApp) capture(orderID string) error {
	status, _, err := a.post(http.MethodPost, a.paypal+"/v2/checkout/orders/"+orderID+"/capture", map[string]any{})
	if err != nil || status < 200 || status > 299 {
		return fmt.Errorf("capture: %v %d", err, status)
	}
	return nil
}

func (a *referenceApp) store(row paymentRow) {
	if a.variant.noRow {
		return
	}
	a.mu.Lock()
	a.memory[row.OrderID] = row
	a.mu.Unlock()
	_, _ = a.db.Exec("REPLACE INTO payments (order_id, contact_id, email, amount, currency, status) VALUES (?, ?, ?, ?, ?, ?)",
		row.OrderID, row.ContactID, row.Email, row.Amount, row.Currency, row.Status)
}

func (a *referenceApp) checkout(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	if a.variant.validateLate {
		_, _, _ = a.post(http.MethodPost, a.hubspot+"/crm/v3/objects/contacts/search", map[string]any{})
	}
	input, ok := parseInput(body)
	if !ok {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	failureStatus := http.StatusBadGateway
	if a.variant.wrongFailureStatus {
		failureStatus = http.StatusInternalServerError
	}
	amount := input.amount.FloatString(2)
	var contactID, orderID string
	var err error
	if a.variant.paypalFirst {
		if orderID, err = a.createOrder(input); err != nil {
			writer.WriteHeader(failureStatus)
			return
		}
	}
	if contactID, err = a.syncContact(input); err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !a.variant.paypalFirst {
		if orderID, err = a.createOrder(input); err != nil {
			if a.variant.rowOnCreateFailure {
				a.store(paymentRow{"NO-ORDER", contactID, input.email, amount, input.currency, "FAILED"})
			}
			writer.WriteHeader(failureStatus)
			return
		}
	}
	row := paymentRow{orderID, contactID, input.email, amount, input.currency, "COMPLETED"}
	if err := a.capture(orderID); err != nil {
		if !a.variant.noFailedRow {
			row.Status = "FAILED"
			a.store(row)
		}
		writer.WriteHeader(failureStatus)
		return
	}
	if a.variant.captureTwice {
		_ = a.capture(orderID)
	}
	a.store(row)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusCreated)
	if a.variant.reorderedResponses {
		_, _ = fmt.Fprintf(writer, `{ "status" : "COMPLETED", "orderId": %q, "contactId": %q }`, orderID, contactID)
		return
	}
	_ = json.NewEncoder(writer).Encode(map[string]string{"contactId": contactID, "orderId": orderID, "status": "COMPLETED"})
}

func (a *referenceApp) getCheckout(writer http.ResponseWriter, request *http.Request) {
	orderID := request.PathValue("orderId")
	var row paymentRow
	if a.variant.memoryGet {
		a.mu.Lock()
		stored, ok := a.memory[orderID]
		a.mu.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		row = stored
	} else {
		err := a.db.QueryRow("SELECT order_id, contact_id, email, CAST(amount AS CHAR), currency, status FROM payments WHERE order_id = ?", orderID).
			Scan(&row.OrderID, &row.ContactID, &row.Email, &row.Amount, &row.Currency, &row.Status)
		if errors.Is(err, sql.ErrNoRows) {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	writer.Header().Set("Content-Type", "application/json")
	amount := json.Number(row.Amount)
	if a.variant.reorderedResponses {
		amount = json.Number(strings.TrimRight(row.Amount, "0") + "0e0")
		_, _ = fmt.Fprintf(writer, `{"status":%q,"currency":%q,"amount":%s,"email":%q,"contactId":%q,"orderId":%q}`,
			row.Status, row.Currency, amount, row.Email, row.ContactID, row.OrderID)
		return
	}
	var encodedAmount any = amount
	if a.variant.stringAmountInGet {
		encodedAmount = row.Amount
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"orderId": row.OrderID, "contactId": row.ContactID, "email": row.Email,
		"amount": encodedAmount, "currency": row.Currency, "status": row.Status,
	})
}
