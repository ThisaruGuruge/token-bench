// Package ordersync is an integration scenario of the grammar benchmark: an
// HTTP service that prices an order through a GraphQL API, screens it with
// FraudLabs Pro, asks a Mistral model for its priority and stores it in a real
// PostgreSQL in one transaction.
package ordersync

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"token-bench/task/internal/intscenario"
)

//go:embed prompt.md
var prompt string

// Prompt is the scenario's task text.
func Prompt() string { return prompt }

// Name is the scenario name.
const Name = "order-sync"

const (
	appPort      = "8091"
	postgresDSN  = "postgres://shop:shop-pass@%s/shop?sslmode=disable&connect_timeout=5"
	requestLimit = 90 * time.Second
	goldTotal    = 40.95
	standardSum  = 17.00
)

const (
	checkStarts   = "program builds and starts"
	checkRejects  = "invalid requests: 400, and nothing is called"
	checkToken    = "missing or wrong API token: 401 before any order logic runs"
	checkCorr     = "correlation id echoed when supplied, made up when missing, on every response"
	checkTiming   = "X-Processing-Time-Ms on every response"
	checkGold     = "gold order: 201 with orderId, discounted total and HIGH priority"
	checkStored   = "orders and their lines stored in PostgreSQL"
	checkStandard = "standard order: full price and NORMAL priority"
	checkPricing  = "one GraphQL request with bearer token and variables"
	checkRetry    = "pricing 503 retried: the order succeeds on the second attempt"
	checkFraud    = "fraud screen with the licence key and the customer's IP"
	checkModel    = "model asked once per accepted order, about its notes"
	checkReject   = "rejected IP and unknown SKU: 422, nothing stored, model not asked"
	checkRollback = "duplicate SKU: 409 and the whole order rolled back"
	checkErrors   = "every error response is {code, message} with its stable code"
)

var checkNames = []string{checkStarts, checkRejects, checkToken, checkCorr, checkTiming, checkGold, checkStandard, checkRetry,
	checkStored, checkPricing, checkFraud, checkModel, checkReject, checkRollback, checkErrors}

const (
	apiToken       = "orders-secret"
	tokenHeader    = "X-Api-Token"
	corrHeader     = "X-Correlation-Id"
	timingHeader   = "X-Processing-Time-Ms"
	retryIP        = "198.51.100.11"
	retryTotal     = 4.25
	expectedOrders = 3
	expectedLines  = 4
)

// errorCodes are the stable codes of the error body, by status.
var errorCodes = map[int]string{400: "INVALID_REQUEST", 401: "UNAUTHORIZED", 409: "CONFLICT", 422: "UNPROCESSABLE", 502: "UPSTREAM_ERROR", 500: "INTERNAL_ERROR"}

const ddl = `
DROP TABLE IF EXISTS order_lines;
DROP TABLE IF EXISTS orders;
CREATE TABLE orders (id SERIAL PRIMARY KEY, customer_id TEXT NOT NULL, total NUMERIC(12,2) NOT NULL,
  priority TEXT NOT NULL, fraud_status TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE order_lines (order_id INT NOT NULL REFERENCES orders(id), sku TEXT NOT NULL, qty INT NOT NULL,
  unit_price NUMERIC(10,2) NOT NULL, PRIMARY KEY (order_id, sku));
`

// Scenario implements intscenario.Scenario.
type Scenario struct{}

func (Scenario) Name() string         { return Name }
func (Scenario) Prompt() string       { return prompt }
func (Scenario) AppPort() string      { return appPort }
func (Scenario) CheckNames() []string { return checkNames }

func (Scenario) Sidecars() []intscenario.Sidecar {
	return []intscenario.Sidecar{
		{Alias: "postgres", Image: "postgres:16-alpine", Port: "5432",
			Env:   []string{"POSTGRES_USER=shop", "POSTGRES_PASSWORD=shop-pass", "POSTGRES_DB=shop"},
			Ready: func(ctx context.Context, addr string) error { return pingPostgres(ctx, addr) }},
		{Alias: "pricing", Mock: true, Ready: ready},
		{Alias: "fraud", Mock: true, Ready: ready},
		{Alias: "mistral", Mock: true, Ready: ready},
	}
}

func ready(ctx context.Context, addr string) error {
	return intscenario.WaitHTTP(ctx, "http://"+addr+"/__health")
}

func pingPostgres(ctx context.Context, addr string) error {
	connection, err := pgx.Connect(ctx, fmt.Sprintf(postgresDSN, addr))
	if err != nil {
		return err
	}
	defer connection.Close(ctx)
	_, err = connection.Exec(ctx, "SELECT 1")
	return err
}

func (Scenario) Reset(ctx context.Context, env *intscenario.Env) error {
	connection, err := pgx.Connect(ctx, fmt.Sprintf(postgresDSN, env.Addr["postgres"]))
	if err != nil {
		return err
	}
	defer connection.Close(ctx)
	if _, err := connection.Exec(ctx, ddl); err != nil {
		return err
	}
	for _, alias := range []string{"pricing", "fraud", "mistral"} {
		if err := env.MockReset(alias); err != nil {
			return err
		}
	}
	return nil
}

func (Scenario) AppReady(ctx context.Context, env *intscenario.Env) error {
	return intscenario.WaitHTTP(ctx, "http://"+env.AppAddr+"/orders")
}

type item struct {
	Sku string `json:"sku"`
	Qty int    `json:"qty"`
}

type orderRequest struct {
	CustomerID string `json:"customerId"`
	IP         string `json:"ip"`
	Items      []item `json:"items"`
	Notes      string `json:"notes,omitempty"`
}

// observation is one response the grader saw.
type observation struct {
	label       string
	status      int
	header      http.Header
	raw         string
	decoded     map[string]any
	correlation string // the id the request carried, "" when none
}

type grader struct {
	env      *intscenario.Env
	checks   *intscenario.Checks
	seen     []observation
	sequence int
}

// post sends an order. token "" uses the valid token, "-" sends none.
func (g *grader) post(label, body, token string, correlation string) (int, map[string]any, string) {
	g.sequence++
	request, _ := http.NewRequest(http.MethodPost, "http://"+g.env.AppAddr+"/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	switch token {
	case "":
		request.Header.Set(tokenHeader, apiToken)
	case "-":
	default:
		request.Header.Set(tokenHeader, token)
	}
	if correlation != "" {
		request.Header.Set(corrHeader, correlation)
	}
	client := http.Client{Timeout: requestLimit}
	response, err := client.Do(request)
	if err != nil {
		g.seen = append(g.seen, observation{label: label, status: 0, raw: err.Error(), correlation: correlation})
		return 0, nil, err.Error()
	}
	defer response.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw.Bytes(), &decoded)
	g.seen = append(g.seen, observation{label: label, status: response.StatusCode, header: response.Header, raw: raw.String(), decoded: decoded, correlation: correlation})
	return response.StatusCode, decoded, raw.String()
}

func order(customer, ip, notes string, items ...item) string {
	encoded, _ := json.Marshal(orderRequest{CustomerID: customer, IP: ip, Items: items, Notes: notes})
	return string(encoded)
}

func (g *grader) rows(ctx context.Context, query string, arguments ...any) ([][]any, error) {
	connection, err := pgx.Connect(ctx, fmt.Sprintf(postgresDSN, g.env.Addr["postgres"]))
	if err != nil {
		return nil, err
	}
	defer connection.Close(ctx)
	result, err := connection.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	var out [][]any
	for result.Next() {
		values, err := result.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, values)
	}
	return out, result.Err()
}

func (g *grader) requests(alias string) []intscenario.Request {
	requests, _ := g.env.MockRequests(alias)
	return requests
}

func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(typed, "%f", &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func numericText(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	}
	var parsed float64
	_, _ = fmt.Sscanf(fmt.Sprint(value), "%f", &parsed)
	return parsed
}

func (Scenario) Grade(ctx context.Context, env *intscenario.Env, checks *intscenario.Checks) {
	g := &grader{env: env, checks: checks}
	g.rejects()
	g.tokens(ctx)
	goldID := g.goldOrder()
	g.standardOrder()
	g.retryOrder()
	g.stored(ctx, goldID)
	g.pricing()
	g.fraud()
	g.rejectedOrders(ctx)
	g.rollback(ctx)
	g.model()
	g.responseRules()
}

func (g *grader) calls() int {
	return len(g.requests("pricing")) + len(g.requests("fraud")) + len(g.requests("mistral"))
}

func (g *grader) rejects() {
	bodies := []string{
		`{"customerId": "c-100", "ip": "198.51.100.7", "items": [`,
		`{"customerId": "", "ip": "198.51.100.7", "items": [{"sku": "A-1", "qty": 1}]}`,
		`{"customerId": "c-100", "ip": "198.51.100.7", "items": []}`,
		`{"customerId": "c-100", "ip": "198.51.100.7", "items": [{"sku": "A-1", "qty": 0}]}`,
		`{"customerId": "c-100", "items": [{"sku": "A-1", "qty": 1}]}`,
	}
	for _, body := range bodies {
		if status, _, raw := g.post("invalid", body, "", ""); status != http.StatusBadRequest {
			g.checks.Fail(checkRejects, fmt.Sprintf("%s answered %d %s", body, status, intscenario.OneLine(raw)))
		}
	}
	if calls := g.calls(); calls > 0 {
		g.checks.Fail(checkRejects, fmt.Sprintf("%d external calls were made for rejected requests", calls))
	}
	g.checks.Pass(checkRejects)
}

func (g *grader) tokens(ctx context.Context) {
	valid := order("c-100", "198.51.100.7", highNote, item{"A-1", 2})
	for _, token := range []string{"-", "wrong-token"} {
		status, _, raw := g.post("unauthorized", valid, token, "corr-unauth-"+token)
		if status != http.StatusUnauthorized {
			g.checks.Fail(checkToken, fmt.Sprintf("token %q answered %d %s", token, status, intscenario.OneLine(raw)))
		}
	}
	if calls := g.calls(); calls > 0 {
		g.checks.Fail(checkToken, fmt.Sprintf("%d external calls were made for unauthorized requests", calls))
	}
	if rows, _ := g.rows(ctx, "SELECT count(*) FROM orders"); fmt.Sprint(rows) != "[[0]]" {
		g.checks.Fail(checkToken, fmt.Sprintf("an order was stored for an unauthorized request: %v", rows))
	}
	g.checks.Pass(checkToken)
}

func (g *grader) goldOrder() float64 {
	status, decoded, raw := g.post("gold", order("c-100", "198.51.100.7", highNote, item{"A-1", 2}, item{"B-7", 1}), "", "corr-gold-1")
	if status != http.StatusCreated {
		g.checks.Fail(checkGold, fmt.Sprintf("answered %d %s", status, intscenario.OneLine(raw)))
		return 0
	}
	id, idOK := number(decoded["orderId"])
	total, totalOK := number(decoded["total"])
	switch {
	case !idOK || id < 1 || id != math.Trunc(id):
		g.checks.Fail(checkGold, "orderId is not a positive integer: "+intscenario.OneLine(raw))
	case !totalOK || math.Abs(total-goldTotal) > 0.005:
		g.checks.Fail(checkGold, fmt.Sprintf("total %v, expected %.2f: %s", decoded["total"], goldTotal, intscenario.OneLine(raw)))
	case decoded["priority"] != "HIGH":
		g.checks.Fail(checkGold, "priority "+fmt.Sprint(decoded["priority"])+", expected HIGH")
	}
	g.checks.Pass(checkGold)
	return id
}

func (g *grader) standardOrder() {
	status, decoded, raw := g.post("standard", order("c-200", "198.51.100.8", normalNote, item{"C-3", 4}), "", "")
	if status != http.StatusCreated {
		g.checks.Fail(checkStandard, fmt.Sprintf("answered %d %s", status, intscenario.OneLine(raw)))
		return
	}
	total, ok := number(decoded["total"])
	if !ok || math.Abs(total-standardSum) > 0.005 || decoded["priority"] != "NORMAL" {
		g.checks.Fail(checkStandard, "body "+intscenario.OneLine(raw)+fmt.Sprintf(", expected total %.2f NORMAL", standardSum))
	}
	g.checks.Pass(checkStandard)
}

func (g *grader) retryOrder() {
	status, decoded, raw := g.post("retry", order(intscenario.PricingFlaky, retryIP, normalNote, item{"C-3", 1}), "", "")
	if status != http.StatusCreated {
		g.checks.Fail(checkRetry, fmt.Sprintf("the order for the flaky customer answered %d %s", status, intscenario.OneLine(raw)))
		return
	}
	if total, ok := number(decoded["total"]); !ok || math.Abs(total-retryTotal) > 0.005 {
		g.checks.Fail(checkRetry, "body "+intscenario.OneLine(raw))
	}
	attempts := 0
	for _, request := range g.requests("pricing") {
		if strings.Contains(request.Body, `"`+intscenario.PricingFlaky+`"`) {
			attempts++
		}
	}
	if attempts != 2 {
		g.checks.Fail(checkRetry, fmt.Sprintf("%d pricing requests for the flaky customer, expected 2 (the 503 and one retry)", attempts))
	}
	g.checks.Pass(checkRetry)
}

func (g *grader) stored(ctx context.Context, goldID float64) {
	orders, err := g.rows(ctx, "SELECT id, customer_id, total::float8, priority, fraud_status FROM orders ORDER BY id")
	if err != nil {
		g.checks.Fail(checkStored, "cannot read orders: "+err.Error())
		return
	}
	if len(orders) != expectedOrders {
		g.checks.Fail(checkStored, fmt.Sprintf("%d orders stored, expected %d", len(orders), expectedOrders))
		return
	}
	first := orders[0]
	if fmt.Sprint(first[1]) != "c-100" || math.Abs(numericText(first[2])-goldTotal) > 0.005 || fmt.Sprint(first[3]) != "HIGH" || fmt.Sprint(first[4]) != "APPROVE" {
		g.checks.Fail(checkStored, fmt.Sprintf("first order row %v, expected c-100 %.2f HIGH APPROVE", first, goldTotal))
	}
	if goldID != 0 && fmt.Sprint(first[0]) != fmt.Sprint(int64(goldID)) {
		g.checks.Fail(checkStored, fmt.Sprintf("the response orderId %v is not the stored id %v", goldID, first[0]))
	}
	lines, err := g.rows(ctx, "SELECT sku, qty, unit_price::float8 FROM order_lines WHERE order_id = $1 ORDER BY sku", first[0])
	if err != nil {
		g.checks.Fail(checkStored, "cannot read lines: "+err.Error())
		return
	}
	want := [][]any{{"A-1", 2, 10.00}, {"B-7", 1, 25.50}}
	if len(lines) != len(want) {
		g.checks.Fail(checkStored, fmt.Sprintf("%d lines stored, expected 2: %v", len(lines), lines))
		return
	}
	for index, line := range lines {
		if fmt.Sprint(line[0]) != want[index][0] || fmt.Sprint(line[1]) != fmt.Sprint(want[index][1]) || math.Abs(numericText(line[2])-want[index][2].(float64)) > 0.005 {
			g.checks.Fail(checkStored, fmt.Sprintf("line %d is %v, expected %v", index, line, want[index]))
		}
	}
	g.checks.Pass(checkStored)
}

func (g *grader) pricing() {
	requests := g.requests("pricing")
	if len(requests) < 3 {
		g.checks.Fail(checkPricing, fmt.Sprintf("%d pricing requests for 3 orders", len(requests)))
		return
	}
	for _, request := range requests {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal([]byte(request.Body), &payload)
		if request.Headers["Authorization"] != "Bearer "+intscenario.PricingToken {
			g.checks.Fail(checkPricing, "Authorization header was "+request.Headers["Authorization"])
		}
		if intscenario.CustomerVariable(payload.Variables) == "" {
			g.checks.Fail(checkPricing, "no customer id variable: "+intscenario.OneLine(request.Body))
		}
		if skus, ok := payload.Variables["skus"].([]any); !ok || len(skus) == 0 {
			g.checks.Fail(checkPricing, "no skus variable: "+intscenario.OneLine(request.Body))
		}
		if strings.Contains(payload.Query, `"c-`) || strings.Contains(payload.Query, `"A-1"`) {
			g.checks.Fail(checkPricing, "values are inlined in the document instead of variables")
		}
	}
	g.checks.Pass(checkPricing)
}

func (g *grader) fraud() {
	ips := map[string]bool{}
	for _, request := range g.requests("fraud") {
		if !strings.Contains(request.Query, "key="+intscenario.FraudKey) {
			g.checks.Fail(checkFraud, "licence key missing from "+request.Query)
		}
		for _, ip := range []string{"198.51.100.7", "198.51.100.8", retryIP} {
			if strings.Contains(request.Query, "ip="+ip) {
				ips[ip] = true
			}
		}
	}
	if len(ips) != 3 {
		g.checks.Fail(checkFraud, fmt.Sprintf("screened %d of the 3 accepted orders' IPs", len(ips)))
	}
	g.checks.Pass(checkFraud)
}

func (g *grader) rejectedOrders(ctx context.Context) {
	modelBefore := len(g.requests("mistral"))
	ordersBefore, _ := g.rows(ctx, "SELECT count(*) FROM orders")
	status, _, raw := g.post("fraud-reject", order("c-100", intscenario.FraudRejectIP, highNote, item{"A-1", 1}), "", "")
	if status != http.StatusUnprocessableEntity {
		g.checks.Fail(checkReject, fmt.Sprintf("rejected IP answered %d %s", status, intscenario.OneLine(raw)))
	}
	status, _, raw = g.post("unknown-sku", order("c-100", "198.51.100.9", highNote, item{"Z-9", 1}), "", "")
	if status != http.StatusUnprocessableEntity {
		g.checks.Fail(checkReject, fmt.Sprintf("unknown SKU answered %d %s", status, intscenario.OneLine(raw)))
	}
	ordersAfter, _ := g.rows(ctx, "SELECT count(*) FROM orders")
	if fmt.Sprint(ordersBefore) != fmt.Sprint(ordersAfter) {
		g.checks.Fail(checkReject, fmt.Sprintf("orders changed from %v to %v", ordersBefore, ordersAfter))
	}
	if after := len(g.requests("mistral")); after != modelBefore {
		g.checks.Fail(checkReject, "the model was asked for a rejected order")
	}
	g.checks.Pass(checkReject)
}

func (g *grader) rollback(ctx context.Context) {
	before, _ := g.rows(ctx, "SELECT count(*) FROM orders")
	status, _, raw := g.post("duplicate-sku", order("c-100", "198.51.100.10", normalNote, item{"A-1", 1}, item{"A-1", 2}), "", "")
	if status != http.StatusConflict {
		g.checks.Fail(checkRollback, fmt.Sprintf("duplicate SKU answered %d %s", status, intscenario.OneLine(raw)))
	}
	after, _ := g.rows(ctx, "SELECT count(*) FROM orders")
	lines, _ := g.rows(ctx, "SELECT count(*) FROM order_lines")
	if fmt.Sprint(before) != fmt.Sprint(after) {
		g.checks.Fail(checkRollback, fmt.Sprintf("an order row survived the rollback: %v -> %v", before, after))
	}
	if fmt.Sprint(lines) != fmt.Sprintf("[[%d]]", expectedLines) {
		g.checks.Fail(checkRollback, fmt.Sprintf("order_lines holds %v rows, expected %d (from the stored orders)", lines, expectedLines))
	}
	g.checks.Pass(checkRollback)
}

func (g *grader) model() {
	counts := map[string]int{}
	for _, request := range g.requests("mistral") {
		for _, note := range []string{highNote, normalNote} {
			if strings.Contains(request.Body, note) {
				counts[note]++
			}
		}
		if !strings.Contains(request.Headers["Authorization"], "mistral-test-key") {
			g.checks.Fail(checkModel, "the API key was not sent: "+request.Headers["Authorization"])
		}
	}
	// Accepted orders: gold (high note), standard and retry (normal note); the
	// duplicate-SKU order is priced, screened and rated before its insert fails.
	if counts[highNote] != 1 {
		g.checks.Fail(checkModel, fmt.Sprintf("the high-priority notes were sent %d times, expected 1", counts[highNote]))
	}
	if counts[normalNote] < 2 || counts[normalNote] > 3 {
		g.checks.Fail(checkModel, fmt.Sprintf("the normal notes were sent %d times, expected 2 or 3", counts[normalNote]))
	}
	g.checks.Pass(checkModel)
}

// responseRules checks the rules that hold for every response the grader saw.
func (g *grader) responseRules() {
	generated := map[string]bool{}
	for _, seen := range g.seen {
		if seen.status == 0 {
			g.checks.Fail(checkCorr, seen.label+": "+seen.raw)
			g.checks.Fail(checkTiming, seen.label+": "+seen.raw)
			g.checks.Fail(checkErrors, seen.label+": "+seen.raw)
			continue
		}
		id := seen.header.Get(corrHeader)
		switch {
		case seen.correlation != "" && id != seen.correlation:
			g.checks.Fail(checkCorr, fmt.Sprintf("%s (status %d): sent %q, the response carried %q", seen.label, seen.status, seen.correlation, id))
		case seen.correlation == "" && strings.TrimSpace(id) == "":
			g.checks.Fail(checkCorr, fmt.Sprintf("%s (status %d): no id was made up", seen.label, seen.status))
		case seen.correlation == "":
			if generated[id] {
				g.checks.Fail(checkCorr, "the made-up id "+id+" was reused")
			}
			generated[id] = true
		}
		if millis, err := strconv.Atoi(strings.TrimSpace(seen.header.Get(timingHeader))); err != nil || millis < 0 {
			g.checks.Fail(checkTiming, fmt.Sprintf("%s (status %d): %s was %q", seen.label, seen.status, timingHeader, seen.header.Get(timingHeader)))
		}
		if seen.status >= 400 {
			code, _ := seen.decoded["code"].(string)
			message, _ := seen.decoded["message"].(string)
			if want, known := errorCodes[seen.status]; !known || code != want || strings.TrimSpace(message) == "" {
				g.checks.Fail(checkErrors, fmt.Sprintf("%s: status %d body %s, expected code %q and a message", seen.label, seen.status, intscenario.OneLine(seen.raw), errorCodes[seen.status]))
			}
		}
	}
	g.checks.Pass(checkCorr)
	g.checks.Pass(checkTiming)
	g.checks.Pass(checkErrors)
}

const (
	highNote   = intscenario.HighNote
	normalNote = intscenario.NormalNote
)

// New prepares a run in workDir on the network, running the program in image.
func New(workDir, image, network string) *intscenario.Handle {
	return intscenario.New(Scenario{}, workDir, image, network)
}
