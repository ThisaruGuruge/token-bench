// Package graphservice is an integration scenario of the grammar benchmark: a
// GraphQL service over a real MySQL with field-level authorization, MQTT
// notifications (a real Mosquitto), Mandrill email and an Ollama model (mocks).
package graphservice

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"token-bench/task/internal/intscenario"
)

//go:embed prompt.md
var prompt string

// Name is the scenario name.
const Name = "graph-service"

const (
	appPort      = "9090"
	endpointPath = "/graphql"
	mysqlDSN     = "shop:shop-pass@tcp(%s)/shop?timeout=5s&multiStatements=true&parseTime=true"
	mqttURL      = "tcp://%s"
	staffToken   = "staff-token"
	adminToken   = "admin-token"
	forbidden    = "forbidden"
	requestLimit = 60 * time.Second
	settle       = 3 * time.Second
	waitLimit    = 20 * time.Second
	pollInterval = 200 * time.Millisecond
	mailSender   = "shop@example.com"
	buyer        = "buyer@example.com"
	modelName    = "llama3.2"
)

const (
	checkStarts   = "program builds, starts and answers GraphQL on 9090 /graphql"
	checkPublic   = "anonymous reads products with id, name, price, description"
	checkStock    = "stock: STAFF and ADMIN only; anonymous gets null, forbidden and the other fields"
	checkCost     = "costPrice: ADMIN only; STAFF gets null and forbidden"
	checkToken    = "an unknown token or another scheme is an anonymous caller"
	checkDenied   = "createProduct by anonymous and STAFF: forbidden and nothing happens"
	checkCreate   = "createProduct by ADMIN stores the product and returns it"
	checkModel    = "the description is written by the model and stored"
	checkCreated  = "products/created published on MQTT with QoS 1"
	checkOrder    = "placeOrder stores the order and reduces the stock in one transaction"
	checkOversell = "placeOrder beyond the stock fails and changes nothing"
	checkEmail    = "order confirmation sent through Mandrill"
	checkPlaced   = "orders/placed published on MQTT with QoS 1"
	checkOrders   = "orders query and Order.customerEmail: STAFF and ADMIN only"
	checkQuiet    = "a failed order sends no email and no MQTT message"
)

var checkNames = []string{checkStarts, checkPublic, checkStock, checkCost, checkToken, checkDenied, checkCreate, checkModel,
	checkCreated, checkOrder, checkOversell, checkEmail, checkPlaced, checkOrders, checkQuiet}

const ddl = `
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS products;
CREATE TABLE products (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100) NOT NULL, price DECIMAL(10,2) NOT NULL,
  cost_price DECIMAL(10,2) NOT NULL, stock INT NOT NULL, description TEXT);
CREATE TABLE orders (id INT AUTO_INCREMENT PRIMARY KEY, product_id INT NOT NULL, quantity INT NOT NULL,
  customer_email VARCHAR(200) NOT NULL, total DECIMAL(12,2) NOT NULL, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP);
INSERT INTO products (name, price, cost_price, stock, description) VALUES
  ('Trail Lantern', 24.50, 11.00, 40, 'Seeded lantern.'),
  ('Camp Stove', 61.00, 30.00, 5, 'Seeded stove.'),
  ('Ridge Backpack', 89.90, 41.00, 12, 'Seeded backpack.');
`

var seedNames = []string{"Trail Lantern", "Camp Stove", "Ridge Backpack"}

// Scenario implements intscenario.Scenario.
type Scenario struct{}

func (Scenario) Name() string         { return Name }
func (Scenario) Prompt() string       { return prompt }
func (Scenario) AppPort() string      { return appPort }
func (Scenario) CheckNames() []string { return checkNames }

func (Scenario) Sidecars() []intscenario.Sidecar {
	return []intscenario.Sidecar{
		{Alias: "mysql", Image: "mysql:8.4", Port: "3306",
			Env:   []string{"MYSQL_ROOT_PASSWORD=root-pass", "MYSQL_DATABASE=shop", "MYSQL_USER=shop", "MYSQL_PASSWORD=shop-pass"},
			Ready: func(_ context.Context, addr string) error { return pingMySQL(addr) }},
		{Alias: "mosquitto", Image: "eclipse-mosquitto:2.0", Args: []string{"mosquitto", "-c", "/mosquitto-no-auth.conf"}, Port: "1883",
			Ready: func(_ context.Context, addr string) error {
				client := mqtt.NewClient(mqtt.NewClientOptions().AddBroker(fmt.Sprintf(mqttURL, addr)).SetClientID("ready"))
				token := client.Connect()
				if !token.WaitTimeout(3*time.Second) || token.Error() != nil {
					return fmt.Errorf("mosquitto not ready: %v", token.Error())
				}
				client.Disconnect(0)
				return nil
			}},
		{Alias: "ollama", Mock: true, Ready: ready},
		{Alias: "mandrill", Mock: true, Ready: ready},
	}
}

func ready(ctx context.Context, addr string) error {
	return intscenario.WaitHTTP(ctx, "http://"+addr+"/__health")
}

func openMySQL(addr string) (*sql.DB, error) {
	return sql.Open("mysql", fmt.Sprintf(mysqlDSN, addr))
}

func pingMySQL(addr string) error {
	db, err := openMySQL(addr)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Ping()
}

func (Scenario) Reset(_ context.Context, env *intscenario.Env) error {
	db, err := openMySQL(env.Addr["mysql"])
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(ddl); err != nil {
		return err
	}
	for _, alias := range []string{"ollama", "mandrill"} {
		if err := env.MockReset(alias); err != nil {
			return err
		}
	}
	return nil
}

func (Scenario) AppReady(_ context.Context, env *intscenario.Env) error {
	_, _, err := post(env, "", `{ __typename }`)
	return err
}

type graphResponse struct {
	Data   map[string]any `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Path    []any  `json:"path"`
	} `json:"errors"`
}

func post(env *intscenario.Env, token, query string) (graphResponse, string, error) {
	encoded, _ := json.Marshal(map[string]any{"query": query})
	request, _ := http.NewRequest(http.MethodPost, "http://"+env.AppAddr+endpointPath, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", token)
	}
	client := http.Client{Timeout: requestLimit}
	response, err := client.Do(request)
	if err != nil {
		return graphResponse{}, "", err
	}
	defer response.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(response.Body)
	var decoded graphResponse
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		return graphResponse{}, raw.String(), fmt.Errorf("not JSON (HTTP %d): %s", response.StatusCode, intscenario.OneLine(raw.String()))
	}
	return decoded, raw.String(), nil
}

func bearer(token string) string { return "Bearer " + token }

type published struct {
	topic   string
	qos     byte
	payload string
}

type grader struct {
	env      *intscenario.Env
	checks   *intscenario.Checks
	db       *sql.DB
	mu       sync.Mutex
	messages []published
}

func (g *grader) mqttMessages() []published {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]published(nil), g.messages...)
}

func (g *grader) ask(token, query string) graphResponse {
	response, raw, err := post(g.env, token, query)
	if err != nil {
		return graphResponse{Errors: []struct {
			Message string `json:"message"`
			Path    []any  `json:"path"`
		}{{Message: "transport: " + err.Error() + " " + raw}}}
	}
	return response
}

// forbiddenAt counts the errors whose message is "forbidden" and whose path ends with field.
func forbiddenAt(response graphResponse, field string) int {
	count := 0
	for _, entry := range response.Errors {
		if entry.Message == forbidden && len(entry.Path) > 0 && fmt.Sprint(entry.Path[len(entry.Path)-1]) == field {
			count++
		}
	}
	return count
}

func errorsText(response graphResponse) string {
	var parts []string
	for _, entry := range response.Errors {
		parts = append(parts, fmt.Sprintf("%s@%v", entry.Message, entry.Path))
	}
	return strings.Join(parts, "; ")
}

func (Scenario) Grade(ctx context.Context, env *intscenario.Env, checks *intscenario.Checks) {
	db, err := openMySQL(env.Addr["mysql"])
	if err != nil {
		checks.Fail(checkStarts, "grader cannot open MySQL: "+err.Error())
		return
	}
	defer db.Close()
	g := &grader{env: env, checks: checks, db: db}
	options := mqtt.NewClientOptions().AddBroker(fmt.Sprintf(mqttURL, env.Addr["mosquitto"])).SetClientID("grader")
	subscriber := mqtt.NewClient(options)
	if token := subscriber.Connect(); !token.WaitTimeout(10*time.Second) || token.Error() != nil {
		checks.Fail(checkStarts, fmt.Sprintf("grader cannot reach MQTT: %v", token.Error()))
		return
	}
	defer subscriber.Disconnect(0)
	for _, topic := range []string{"products/#", "orders/#"} {
		subscriber.Subscribe(topic, 2, func(_ mqtt.Client, message mqtt.Message) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.messages = append(g.messages, published{topic: message.Topic(), qos: message.Qos(), payload: string(message.Payload())})
		}).Wait()
	}
	g.reads()
	g.denied()
	g.created()
	g.ordering()
	g.oversell()
	g.orders()
}

func (g *grader) count(query string, args ...any) int {
	var n int
	if err := g.db.QueryRow(query, args...).Scan(&n); err != nil {
		return -1
	}
	return n
}

func number(value any) (float64, bool) {
	typed, ok := value.(float64)
	return typed, ok
}

func (g *grader) reads() {
	response := g.ask("", `{ products { id name price description } }`)
	products, _ := response.Data["products"].([]any)
	if len(response.Errors) > 0 || len(products) != len(seedNames) {
		g.checks.Fail(checkPublic, fmt.Sprintf("%d products, errors: %s", len(products), errorsText(response)))
	}
	for index, item := range products {
		row, _ := item.(map[string]any)
		price, priceOK := number(row["price"])
		if index < len(seedNames) && (row["name"] != seedNames[index] || !priceOK || price <= 0 || fmt.Sprint(row["id"]) != fmt.Sprint(index+1) || row["description"] == nil) {
			g.checks.Fail(checkPublic, fmt.Sprintf("product %d is %v", index+1, row))
		}
	}
	g.checks.Pass(checkPublic)

	g.stockChecks()
	g.costChecks()
	g.tokenChecks()
}

func (g *grader) stockChecks() {
	query := `{ products { name stock } }`
	anonymous := g.ask("", query)
	if forbiddenAt(anonymous, "stock") != len(seedNames) {
		g.checks.Fail(checkStock, "anonymous: expected one forbidden error per product at stock, got: "+errorsText(anonymous))
	}
	products, _ := anonymous.Data["products"].([]any)
	if len(products) != len(seedNames) {
		g.checks.Fail(checkStock, "anonymous: the other fields were not answered: "+fmt.Sprint(anonymous.Data))
	}
	for _, item := range products {
		row, _ := item.(map[string]any)
		if row["stock"] != nil || row["name"] == nil {
			g.checks.Fail(checkStock, fmt.Sprintf("anonymous: row %v", row))
		}
	}
	for _, token := range []string{staffToken, adminToken} {
		response := g.ask(bearer(token), query)
		products, _ := response.Data["products"].([]any)
		if len(response.Errors) > 0 || len(products) != len(seedNames) {
			g.checks.Fail(checkStock, token+": "+errorsText(response))
			continue
		}
		first, _ := products[0].(map[string]any)
		if value, ok := number(first["stock"]); !ok || value != 40 {
			g.checks.Fail(checkStock, fmt.Sprintf("%s: stock of the first product is %v, expected 40", token, first["stock"]))
		}
	}
	g.checks.Pass(checkStock)
}

func (g *grader) costChecks() {
	query := `{ product(id: 1) { name stock costPrice } }`
	staff := g.ask(bearer(staffToken), query)
	product, _ := staff.Data["product"].(map[string]any)
	if forbiddenAt(staff, "costPrice") != 1 || len(staff.Errors) != 1 || product == nil || product["costPrice"] != nil {
		g.checks.Fail(checkCost, "STAFF: expected costPrice null with one forbidden error: "+intscenarioJSON(staff.Data)+" "+errorsText(staff))
	} else if value, ok := number(product["stock"]); !ok || value != 40 {
		g.checks.Fail(checkCost, fmt.Sprintf("STAFF: stock %v, expected 40", product["stock"]))
	}
	admin := g.ask(bearer(adminToken), query)
	product, _ = admin.Data["product"].(map[string]any)
	if value, ok := number(product["costPrice"]); len(admin.Errors) > 0 || !ok || math.Abs(value-11.0) > 0.005 {
		g.checks.Fail(checkCost, "ADMIN: "+intscenarioJSON(admin.Data)+" "+errorsText(admin))
	}
	g.checks.Pass(checkCost)
}

func intscenarioJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return intscenario.OneLine(string(encoded))
}

func (g *grader) tokenChecks() {
	for _, header := range []string{bearer("not-a-token"), "Basic " + staffToken, staffToken, "Bearer"} {
		response := g.ask(header, `{ products { name stock } }`)
		if forbiddenAt(response, "stock") != len(seedNames) {
			g.checks.Fail(checkToken, fmt.Sprintf("Authorization %q was not treated as anonymous: %s", header, errorsText(response)))
		}
	}
	g.checks.Pass(checkToken)
}

const createMutation = `mutation { createProduct(name: "Summit Bottle", price: 18.0, costPrice: 7.25, stock: 60) { id name price description stock costPrice } }`

func (g *grader) denied() {
	modelBefore, mailBefore := len(g.requests("ollama")), len(g.requests("mandrill"))
	for _, token := range []string{"", bearer(staffToken)} {
		response := g.ask(token, createMutation)
		if forbiddenAt(response, "createProduct") != 1 || response.Data["createProduct"] != nil {
			g.checks.Fail(checkDenied, fmt.Sprintf("token %q: data %v errors %s", token, response.Data, errorsText(response)))
		}
	}
	time.Sleep(settle)
	switch {
	case g.count("SELECT count(*) FROM products") != len(seedNames):
		g.checks.Fail(checkDenied, "a product was inserted for a caller who may not create one")
	case len(g.requests("ollama")) != modelBefore:
		g.checks.Fail(checkDenied, "the model was asked for a denied createProduct")
	case len(g.mqttMessages()) != 0:
		g.checks.Fail(checkDenied, "an MQTT message was sent for a denied createProduct")
	case len(g.requests("mandrill")) != mailBefore:
		g.checks.Fail(checkDenied, "an email was sent for a denied createProduct")
	}
	g.checks.Pass(checkDenied)
}

func (g *grader) requests(alias string) []intscenario.Request {
	requests, _ := g.env.MockRequests(alias)
	return requests
}

func (g *grader) created() {
	response := g.ask(bearer(adminToken), createMutation)
	product, _ := response.Data["createProduct"].(map[string]any)
	if len(response.Errors) > 0 || product == nil {
		g.checks.Fail(checkCreate, "ADMIN createProduct: "+errorsText(response)+" "+intscenarioJSON(response.Data))
		g.checks.Fail(checkModel, "no product created")
		g.checks.Fail(checkCreated, "no product created")
		return
	}
	price, _ := number(product["price"])
	stock, _ := number(product["stock"])
	cost, _ := number(product["costPrice"])
	if product["name"] != "Summit Bottle" || math.Abs(price-18) > 0.005 || stock != 60 || math.Abs(cost-7.25) > 0.005 {
		g.checks.Fail(checkCreate, "returned product "+intscenarioJSON(product))
	}
	var name, description string
	var storedPrice, storedCost float64
	var storedStock int
	err := g.db.QueryRow("SELECT name, price, cost_price, stock, COALESCE(description, '') FROM products WHERE name = 'Summit Bottle'").
		Scan(&name, &storedPrice, &storedCost, &storedStock, &description)
	if err != nil {
		g.checks.Fail(checkCreate, "no stored row: "+err.Error())
		g.checks.Fail(checkModel, "no stored row")
	} else {
		if math.Abs(storedPrice-18) > 0.005 || math.Abs(storedCost-7.25) > 0.005 || storedStock != 60 {
			g.checks.Fail(checkCreate, fmt.Sprintf("stored row %s %v %v %d", name, storedPrice, storedCost, storedStock))
		}
		want := intscenario.ProductDescriptions["Summit Bottle"]
		if description != want {
			g.checks.Fail(checkModel, fmt.Sprintf("stored description %q, expected the model's %q", description, want))
		}
		if product["description"] != want {
			g.checks.Fail(checkModel, fmt.Sprintf("returned description %v, expected the model's", product["description"]))
		}
	}
	requests := g.requests("ollama")
	if len(requests) != 1 {
		g.checks.Fail(checkModel, fmt.Sprintf("the model was asked %d times for one product, expected 1", len(requests)))
	} else if !strings.Contains(requests[0].Body, "Summit Bottle") || !strings.Contains(requests[0].Body, modelName) {
		g.checks.Fail(checkModel, "the model request names neither the product nor the model: "+intscenario.OneLine(requests[0].Body))
	}
	g.checks.Pass(checkCreate)
	g.checks.Pass(checkModel)
	id, _ := number(product["id"])
	if id == 0 {
		idText := fmt.Sprint(product["id"])
		fmt.Sscanf(idText, "%f", &id)
	}
	g.expectMessage(checkCreated, "products/created", func(payload map[string]any) string {
		gotID, ok := number(payload["id"])
		if !ok || gotID != id || payload["name"] != "Summit Bottle" {
			return fmt.Sprintf("payload %v, expected id %v and name Summit Bottle", payload, id)
		}
		return ""
	})
}

// expectMessage waits for exactly one message on topic and validates it.
func (g *grader) expectMessage(check, topic string, valid func(map[string]any) string) {
	found := func() []published {
		var matches []published
		for _, message := range g.mqttMessages() {
			if message.topic == topic {
				matches = append(matches, message)
			}
		}
		return matches
	}
	deadline := time.Now().Add(waitLimit)
	for len(found()) == 0 && time.Now().Before(deadline) {
		time.Sleep(pollInterval)
	}
	time.Sleep(settle)
	matches := found()
	if len(matches) != 1 {
		g.checks.Fail(check, fmt.Sprintf("%d messages on %s, expected 1", len(matches), topic))
		return
	}
	if matches[0].qos != 1 {
		g.checks.Fail(check, fmt.Sprintf("QoS %d, expected 1", matches[0].qos))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(matches[0].payload), &payload); err != nil {
		g.checks.Fail(check, "payload is not a JSON object: "+matches[0].payload)
		return
	}
	if problem := valid(payload); problem != "" {
		g.checks.Fail(check, problem)
	}
	g.checks.Pass(check)
}

const placeMutation = `mutation { placeOrder(productId: 1, quantity: 3, customerEmail: "` + buyer + `") { id quantity total product { name } customerEmail } }`

func (g *grader) ordering() {
	response := g.ask("", placeMutation)
	order, _ := response.Data["placeOrder"].(map[string]any)
	if order == nil {
		g.checks.Fail(checkOrder, "placeOrder: "+errorsText(response)+" "+intscenarioJSON(response.Data))
		g.checks.Fail(checkEmail, "no order placed")
		g.checks.Fail(checkPlaced, "no order placed")
		g.checks.Fail(checkOrders, "no order placed")
		return
	}
	if forbiddenAt(response, "customerEmail") != 1 || order["customerEmail"] != nil {
		g.checks.Fail(checkOrders, "anonymous placeOrder: customerEmail must be null with one forbidden error: "+errorsText(response))
	}
	total, _ := number(order["total"])
	product, _ := order["product"].(map[string]any)
	if math.Abs(total-73.5) > 0.005 || product["name"] != "Trail Lantern" {
		g.checks.Fail(checkOrder, "returned order "+intscenarioJSON(order))
	}
	var stock int
	_ = g.db.QueryRow("SELECT stock FROM products WHERE id = 1").Scan(&stock)
	var productID, quantity int
	var email string
	var storedTotal float64
	err := g.db.QueryRow("SELECT product_id, quantity, customer_email, total FROM orders").Scan(&productID, &quantity, &email, &storedTotal)
	switch {
	case g.count("SELECT count(*) FROM orders") != 1 || err != nil:
		g.checks.Fail(checkOrder, fmt.Sprintf("orders table: %d rows (%v)", g.count("SELECT count(*) FROM orders"), err))
	case productID != 1 || quantity != 3 || email != buyer || math.Abs(storedTotal-73.5) > 0.005:
		g.checks.Fail(checkOrder, fmt.Sprintf("stored order %d %d %s %v", productID, quantity, email, storedTotal))
	case stock != 37:
		g.checks.Fail(checkOrder, fmt.Sprintf("stock is %d, expected 37", stock))
	}
	g.checks.Pass(checkOrder)
	id := order["id"]
	g.mail(id)
	g.expectMessage(checkPlaced, "orders/placed", func(payload map[string]any) string {
		orderID, idOK := number(payload["orderId"])
		productID, productOK := number(payload["productId"])
		quantity, quantityOK := number(payload["quantity"])
		if !idOK || fmt.Sprint(orderID) != fmt.Sprint(id) && fmt.Sprintf("%.0f", orderID) != fmt.Sprint(id) || !productOK || productID != 1 || !quantityOK || quantity != 3 {
			return fmt.Sprintf("payload %v, expected orderId %v productId 1 quantity 3", payload, id)
		}
		return ""
	})
}

func (g *grader) mail(id any) {
	requests := g.requests("mandrill")
	if len(requests) != 1 {
		g.checks.Fail(checkEmail, fmt.Sprintf("%d Mandrill requests, expected 1", len(requests)))
		return
	}
	var payload struct {
		Key     string `json:"key"`
		Message struct {
			FromEmail string `json:"from_email"`
			Subject   string `json:"subject"`
			Text      string `json:"text"`
			HTML      string `json:"html"`
			To        []struct {
				Email string `json:"email"`
			} `json:"to"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(requests[0].Body), &payload); err != nil {
		g.checks.Fail(checkEmail, "body is not JSON: "+intscenario.OneLine(requests[0].Body))
		return
	}
	message := payload.Message
	switch {
	case payload.Key != intscenario.MandrillKey:
		g.checks.Fail(checkEmail, "key "+payload.Key)
	case message.FromEmail != mailSender:
		g.checks.Fail(checkEmail, "from_email "+message.FromEmail)
	case len(message.To) != 1 || message.To[0].Email != buyer:
		g.checks.Fail(checkEmail, fmt.Sprintf("recipients %v", message.To))
	case message.Subject != fmt.Sprintf("Order %v confirmed", id):
		g.checks.Fail(checkEmail, fmt.Sprintf("subject %q, expected order %v confirmed", message.Subject, id))
	case !strings.Contains(message.Text+message.HTML, "Trail Lantern"):
		g.checks.Fail(checkEmail, "the text does not name the product: "+intscenario.OneLine(message.Text+message.HTML))
	}
	g.checks.Pass(checkEmail)
}

func (g *grader) oversell() {
	mailBefore, messagesBefore := len(g.requests("mandrill")), len(g.mqttMessages())
	response := g.ask("", `mutation { placeOrder(productId: 2, quantity: 6, customerEmail: "`+buyer+`") { id } }`)
	if response.Data["placeOrder"] != nil || len(response.Errors) == 0 {
		g.checks.Fail(checkOversell, "expected a null placeOrder with an error: "+intscenarioJSON(response.Data)+" "+errorsText(response))
	}
	var stock int
	_ = g.db.QueryRow("SELECT stock FROM products WHERE id = 2").Scan(&stock)
	if stock != 5 || g.count("SELECT count(*) FROM orders") != 1 {
		g.checks.Fail(checkOversell, fmt.Sprintf("stock of product 2 is %d (expected 5), orders %d (expected 1)", stock, g.count("SELECT count(*) FROM orders")))
	}
	g.checks.Pass(checkOversell)
	time.Sleep(settle)
	if len(g.requests("mandrill")) != mailBefore {
		g.checks.Fail(checkQuiet, "an email was sent for the failed order")
	}
	if len(g.mqttMessages()) != messagesBefore {
		g.checks.Fail(checkQuiet, "an MQTT message was sent for the failed order")
	}
	g.checks.Pass(checkQuiet)
}

func (g *grader) orders() {
	query := `{ orders { id quantity total customerEmail product { name } } }`
	anonymous := g.ask("", query)
	if anonymous.Data["orders"] != nil || forbiddenAt(anonymous, "orders") != 1 {
		g.checks.Fail(checkOrders, "anonymous orders: expected null and one forbidden error: "+intscenarioJSON(anonymous.Data)+" "+errorsText(anonymous))
	}
	for _, token := range []string{staffToken, adminToken} {
		response := g.ask(bearer(token), query)
		orders, _ := response.Data["orders"].([]any)
		if len(response.Errors) > 0 || len(orders) != 1 {
			g.checks.Fail(checkOrders, token+": "+errorsText(response)+" "+intscenarioJSON(response.Data))
			continue
		}
		row, _ := orders[0].(map[string]any)
		if row["customerEmail"] != buyer {
			g.checks.Fail(checkOrders, fmt.Sprintf("%s: customerEmail %v", token, row["customerEmail"]))
		}
	}
	g.checks.Pass(checkOrders)
}

// New prepares a run in workDir on the network, running the program in image.
func New(workDir, image, network string) *intscenario.Handle {
	return intscenario.New(Scenario{}, workDir, image, network)
}

// Prompt is the scenario's task text.
func Prompt() string { return prompt }
