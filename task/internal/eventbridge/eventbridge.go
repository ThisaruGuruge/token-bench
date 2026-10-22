// Package eventbridge is an integration scenario of the grammar benchmark: a
// bridge that consumes order events from a real RabbitMQ queue, republishes
// the good ones to a real NATS JetStream stream, dead-letters the poison ones
// and raises a PagerDuty incident (a mock) when the third one arrives.
package eventbridge

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	amqp "github.com/rabbitmq/amqp091-go"

	"token-bench/task/internal/intscenario"
)

//go:embed prompt.md
var prompt string

// Name is the scenario name.
const Name = "event-bridge"

const (
	incomingQueue = "orders.incoming"
	deadQueue     = "orders.dead"
	deadExchange  = "orders.dlx"
	streamName    = "ORDERS"
	subjectFilter = "orders.created.*"
	maxEvents     = 100000
	retention     = 24 * time.Hour
	incidentTitle = "Order bridge: 3 poison messages dead-lettered"
	incidentFrom  = "ops@example.com"
	waitLimit     = 40 * time.Second
	quietPeriod   = 4 * time.Second
	amqpURL       = "amqp://guest:guest@%s/"
	natsURL       = "nats://%s"
	pollInterval  = 300 * time.Millisecond
)

const (
	checkStarts    = "program builds, starts and consumes orders.incoming"
	checkStream    = "stream ORDERS exists and collects orders.created.*"
	checkStorage   = "stream keeps events on disk for 24 hours, at most 100000"
	checkRepublish = "every good event is republished once with its body unchanged"
	checkSubject   = "subject is orders.created.<currency in lower case>"
	checkOrder     = "events reach the stream in the order they were read"
	checkAcked     = "good events are acknowledged: nothing left or unacknowledged, none dead-lettered"
	checkMalformed = "a body that is not JSON is dead-lettered by rejection, body intact"
	checkInvalid   = "events that break the rules are dead-lettered by rejection, body intact"
	checkOnce      = "poison messages are not redelivered and never reach the stream"
	checkNoEarly   = "no PagerDuty incident before the third poison message"
	checkOneAlert  = "exactly one incident at the third poison message, none for the fourth"
	checkIncident  = "incident has the title, the service, high urgency and the From user"
	checkToken     = "incident request carries the PagerDuty API key"
	checkResilient = "the bridge keeps consuming after poison messages"
)

var checkNames = []string{checkStarts, checkStream, checkStorage, checkRepublish, checkSubject, checkOrder, checkAcked,
	checkMalformed, checkInvalid, checkOnce, checkNoEarly, checkOneAlert, checkIncident, checkToken, checkResilient}

type message struct {
	body    string
	subject string // the expected stream subject, "" for poison
}

var (
	good = []message{
		{`{"orderId":"o-1001","customerId":"acme","amount":125.5,"currency":"EUR"}`, "orders.created.eur"},
		{`{"orderId":"o-1002","customerId":"globex","amount":40,"currency":"USD"}`, "orders.created.usd"},
		{`{"orderId":"o-1003","customerId":"initech","amount":9.99,"currency":"GBP","channel":"web"}`, "orders.created.gbp"},
		{`{"orderId":"o-1004","customerId":"acme","amount":310.25,"currency":"EUR"}`, "orders.created.eur"},
		{`{"orderId":"o-1005","customerId":"globex","amount":7,"currency":"USD"}`, "orders.created.usd"},
	}
	afterwards = message{`{"orderId":"o-1006","customerId":"initech","amount":1250,"currency":"JPY"}`, "orders.created.jpy"}
	malformed  = `{"orderId":"o-2001","customerId":"acme","amount":`
	invalid    = []string{
		`{"orderId":"o-2002","amount":10.0,"currency":"EUR"}`,
		`{"orderId":"o-2003","customerId":"acme","amount":-5,"currency":"EUR"}`,
		`{"orderId":"o-2004","customerId":"acme","amount":20,"currency":"euro"}`,
	}
)

// Scenario implements intscenario.Scenario.
type Scenario struct{}

func (Scenario) Name() string         { return Name }
func (Scenario) Prompt() string       { return prompt }
func (Scenario) AppPort() string      { return "" }
func (Scenario) CheckNames() []string { return checkNames }

func (Scenario) Sidecars() []intscenario.Sidecar {
	return []intscenario.Sidecar{
		{Alias: "rabbitmq", Image: "rabbitmq:3.13-alpine", Port: "5672", Arm64: true, Ready: func(_ context.Context, addr string) error {
			connection, err := amqp.Dial(fmt.Sprintf(amqpURL, addr))
			if err != nil {
				return err
			}
			return connection.Close()
		}},
		{Alias: "nats", Image: "nats:2.10-alpine", Args: []string{"-js"}, Port: "4222", Ready: func(_ context.Context, addr string) error {
			connection, err := nats.Connect(fmt.Sprintf(natsURL, addr))
			if err != nil {
				return err
			}
			connection.Close()
			return nil
		}},
		{Alias: "pagerduty", Mock: true, Ready: func(ctx context.Context, addr string) error {
			return intscenario.WaitHTTP(ctx, "http://"+addr+"/__health")
		}},
	}
}

func (Scenario) Reset(_ context.Context, env *intscenario.Env) error {
	connection, err := amqp.Dial(fmt.Sprintf(amqpURL, env.Addr["rabbitmq"]))
	if err != nil {
		return err
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		return err
	}
	for _, queue := range []string{incomingQueue, deadQueue} {
		if _, err := channel.QueueDelete(queue, false, false, false); err != nil {
			return err
		}
	}
	channel, err = connection.Channel()
	if err != nil {
		return err
	}
	if err := channel.ExchangeDeclare(deadExchange, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := channel.QueueDeclare(deadQueue, true, false, false, false, nil); err != nil {
		return err
	}
	if err := channel.QueueBind(deadQueue, deadQueue, deadExchange, false, nil); err != nil {
		return err
	}
	client, err := nats.Connect(fmt.Sprintf(natsURL, env.Addr["nats"]))
	if err != nil {
		return err
	}
	defer client.Close()
	streams, err := client.JetStream()
	if err != nil {
		return err
	}
	if err := streams.DeleteStream(streamName); err != nil && err != nats.ErrStreamNotFound {
		return err
	}
	return env.MockReset("pagerduty")
}

func (Scenario) AppReady(_ context.Context, env *intscenario.Env) error {
	client, err := nats.Connect(fmt.Sprintf(natsURL, env.Addr["nats"]))
	if err != nil {
		return err
	}
	defer client.Close()
	streams, err := client.JetStream()
	if err != nil {
		return err
	}
	if _, err := streams.StreamInfo(streamName); err != nil {
		return fmt.Errorf("stream %s: %w", streamName, err)
	}
	connection, err := amqp.Dial(fmt.Sprintf(amqpURL, env.Addr["rabbitmq"]))
	if err != nil {
		return err
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		return err
	}
	queue, err := channel.QueueDeclarePassive(incomingQueue, true, false, false, false, nil)
	if err != nil {
		return err
	}
	if queue.Consumers == 0 {
		return fmt.Errorf("nothing consumes %s yet", incomingQueue)
	}
	return nil
}

type grader struct {
	env    *intscenario.Env
	checks *intscenario.Checks
	js     nats.JetStreamContext
	conn   *amqp.Connection
	ch     *amqp.Channel
}

func (Scenario) Grade(ctx context.Context, env *intscenario.Env, checks *intscenario.Checks) {
	client, err := nats.Connect(fmt.Sprintf(natsURL, env.Addr["nats"]))
	if err != nil {
		checks.Fail(checkStarts, "grader cannot reach NATS: "+err.Error())
		return
	}
	defer client.Close()
	streams, _ := client.JetStream()
	connection, err := amqp.Dial(fmt.Sprintf(amqpURL, env.Addr["rabbitmq"]))
	if err != nil {
		checks.Fail(checkStarts, "grader cannot reach RabbitMQ: "+err.Error())
		return
	}
	defer connection.Close()
	channel, _ := connection.Channel()
	g := &grader{env: env, checks: checks, js: streams, conn: connection, ch: channel}
	g.streamConfig()
	g.goodEvents()
	g.poisonBelowThreshold()
	g.poisonAtThreshold()
	g.afterwards()
	g.finish()
}

func (g *grader) publish(body string) error {
	return g.ch.PublishWithContext(context.Background(), "", incomingQueue, false, false,
		amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: []byte(body)})
}

func (g *grader) streamConfig() {
	info, err := g.js.StreamInfo(streamName)
	if err != nil {
		g.checks.Fail(checkStream, err.Error())
		g.checks.Fail(checkStorage, err.Error())
		return
	}
	if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != subjectFilter {
		g.checks.Fail(checkStream, fmt.Sprintf("subjects %v, expected [%s]", info.Config.Subjects, subjectFilter))
	}
	g.checks.Pass(checkStream)
	if info.Config.Storage != nats.FileStorage {
		g.checks.Fail(checkStorage, fmt.Sprintf("storage %v, expected file", info.Config.Storage))
	}
	if info.Config.MaxAge != retention {
		g.checks.Fail(checkStorage, fmt.Sprintf("max age %v, expected %v", info.Config.MaxAge, retention))
	}
	if info.Config.MaxMsgs != maxEvents {
		g.checks.Fail(checkStorage, fmt.Sprintf("max messages %d, expected %d", info.Config.MaxMsgs, maxEvents))
	}
	g.checks.Pass(checkStorage)
}

// stored returns what the stream holds, in sequence order.
func (g *grader) stored() ([]nats.RawStreamMsg, error) {
	info, err := g.js.StreamInfo(streamName)
	if err != nil {
		return nil, err
	}
	var out []nats.RawStreamMsg
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq && info.State.Msgs > 0; sequence++ {
		raw, err := g.js.GetMsg(streamName, sequence)
		if err != nil {
			return nil, err
		}
		out = append(out, nats.RawStreamMsg{Subject: raw.Subject, Data: raw.Data})
	}
	return out, nil
}

func until(condition func() bool) bool {
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

func (g *grader) queueCounts() (ready, unacked int, ok bool) {
	output, err := g.env.Exec(context.Background(), "rabbitmq", "rabbitmqctl", "list_queues", "-q", "name", "messages_ready", "messages_unacknowledged")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(output, "\n") {
		var name string
		var r, u int
		if n, _ := fmt.Sscanf(line, "%s %d %d", &name, &r, &u); n == 3 && name == incomingQueue {
			return r, u, true
		}
	}
	return 0, 0, false
}

func (g *grader) deadCount() int {
	queue, err := g.ch.QueueDeclarePassive(deadQueue, true, false, false, false, nil)
	if err != nil {
		g.ch, _ = g.conn.Channel()
		return -1
	}
	return queue.Messages
}

func (g *grader) goodEvents() {
	for _, event := range good {
		if err := g.publish(event.body); err != nil {
			g.checks.Fail(checkRepublish, "grader cannot publish: "+err.Error())
			return
		}
	}
	got := until(func() bool { stored, _ := g.stored(); return len(stored) >= len(good) })
	stored, _ := g.stored()
	if !got {
		g.checks.Fail(checkRepublish, fmt.Sprintf("%d of %d events reached the stream within %s", len(stored), len(good), waitLimit))
		g.checks.Fail(checkSubject, "no events to inspect")
		g.checks.Fail(checkOrder, "no events to inspect")
		return
	}
	time.Sleep(quietPeriod)
	stored, _ = g.stored()
	if len(stored) != len(good) {
		g.checks.Fail(checkRepublish, fmt.Sprintf("%d events in the stream, expected %d", len(stored), len(good)))
	}
	for index, event := range good {
		if index >= len(stored) {
			break
		}
		if !bytes.Equal(stored[index].Data, []byte(event.body)) {
			g.checks.Fail(checkRepublish, fmt.Sprintf("stream message %d is %s", index+1, intscenario.OneLine(string(stored[index].Data))))
		}
		if stored[index].Subject != event.subject {
			g.checks.Fail(checkSubject, fmt.Sprintf("message %d (%s) is on %s, expected %s", index+1, event.body[:20], stored[index].Subject, event.subject))
		}
	}
	g.checks.Pass(checkRepublish)
	g.checks.Pass(checkSubject)
	g.checks.Pass(checkOrder)
}

func (g *grader) incidents() []intscenario.Request {
	requests, _ := g.env.MockRequests("pagerduty")
	return requests
}

func (g *grader) poisonBelowThreshold() {
	for _, body := range []string{malformed, invalid[0]} {
		if err := g.publish(body); err != nil {
			g.checks.Fail(checkMalformed, "grader cannot publish: "+err.Error())
			return
		}
	}
	until(func() bool { return g.deadCount() >= 2 })
	time.Sleep(quietPeriod)
	if requests := g.incidents(); len(requests) != 0 {
		g.checks.Fail(checkNoEarly, fmt.Sprintf("%d incident(s) after two poison messages", len(requests)))
	}
	g.checks.Pass(checkNoEarly)
}

func (g *grader) poisonAtThreshold() {
	if err := g.publish(invalid[1]); err != nil {
		g.checks.Fail(checkOneAlert, "grader cannot publish: "+err.Error())
		return
	}
	until(func() bool { return g.deadCount() >= 3 && len(g.incidents()) >= 1 })
	if err := g.publish(invalid[2]); err != nil {
		g.checks.Fail(checkOneAlert, "grader cannot publish: "+err.Error())
		return
	}
	until(func() bool { return g.deadCount() >= 4 })
	time.Sleep(quietPeriod)
	requests := g.incidents()
	switch {
	case len(requests) == 0:
		g.checks.Fail(checkOneAlert, "no PagerDuty request after the third poison message")
		g.checks.Fail(checkIncident, "no incident to inspect")
		g.checks.Fail(checkToken, "no incident to inspect")
		return
	case len(requests) != 1:
		g.checks.Fail(checkOneAlert, fmt.Sprintf("%d PagerDuty requests, expected exactly 1", len(requests)))
	}
	g.checks.Pass(checkOneAlert)
	request := requests[0]
	if request.Headers["Authorization"] != intscenario.PagerDutyToken {
		g.checks.Fail(checkToken, "Authorization was "+request.Headers["Authorization"])
	}
	g.checks.Pass(checkToken)
	g.inspectIncident(request)
}

func (g *grader) inspectIncident(request intscenario.Request) {
	var payload struct {
		Incident struct {
			Type    string `json:"type"`
			Title   string `json:"title"`
			Urgency string `json:"urgency"`
			Service struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"service"`
		} `json:"incident"`
	}
	if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
		g.checks.Fail(checkIncident, "body is not JSON: "+intscenario.OneLine(request.Body))
		return
	}
	incident := payload.Incident
	switch {
	case request.Headers["From"] != incidentFrom:
		g.checks.Fail(checkIncident, "From header was "+request.Headers["From"])
	case incident.Title != incidentTitle:
		g.checks.Fail(checkIncident, fmt.Sprintf("title %q, expected %q", incident.Title, incidentTitle))
	case incident.Service.ID != intscenario.PagerDutyService:
		g.checks.Fail(checkIncident, "service "+incident.Service.ID)
	case incident.Urgency != "high":
		g.checks.Fail(checkIncident, fmt.Sprintf("urgency %q, expected high", incident.Urgency))
	}
	g.checks.Pass(checkIncident)
}

func (g *grader) afterwards() {
	if err := g.publish(afterwards.body); err != nil {
		g.checks.Fail(checkResilient, "grader cannot publish: "+err.Error())
		return
	}
	reached := until(func() bool { stored, _ := g.stored(); return len(stored) >= len(good)+1 })
	if !reached {
		g.checks.Fail(checkResilient, "the good event after the poison messages never reached the stream")
		return
	}
	g.checks.Pass(checkResilient)
}

func (g *grader) finish() {
	drained := until(func() bool {
		ready, unacked, ok := g.queueCounts()
		return ok && ready == 0 && unacked == 0
	})
	if !drained {
		ready, unacked, _ := g.queueCounts()
		g.checks.Fail(checkAcked, fmt.Sprintf("%s still holds %d ready and %d unacknowledged messages", incomingQueue, ready, unacked))
	}
	stored, _ := g.stored()
	if len(stored) != len(good)+1 {
		g.checks.Fail(checkOnce, fmt.Sprintf("the stream holds %d events, expected %d (poison messages must not reach it)", len(stored), len(good)+1))
	}
	dead := map[string]amqp.Delivery{}
	count := 0
	for {
		delivery, ok, err := g.ch.Get(deadQueue, true)
		if err != nil || !ok {
			break
		}
		count++
		if _, repeated := dead[string(delivery.Body)]; repeated {
			g.checks.Fail(checkOnce, "dead-lettered twice: "+intscenario.OneLine(string(delivery.Body)))
		}
		dead[string(delivery.Body)] = delivery
	}
	if count != 4 {
		g.checks.Fail(checkOnce, fmt.Sprintf("%s holds %d messages, expected the 4 poison messages", deadQueue, count))
	}
	for _, event := range good {
		if _, found := dead[event.body]; found {
			g.checks.Fail(checkAcked, "a good event was dead-lettered: "+event.body)
		}
	}
	g.checks.Pass(checkAcked)
	g.deadLettered(dead, []string{malformed}, checkMalformed)
	g.deadLettered(dead, invalid, checkInvalid)
	g.checks.Pass(checkOnce)
}

func (g *grader) deadLettered(dead map[string]amqp.Delivery, bodies []string, check string) {
	for _, body := range bodies {
		delivery, found := dead[body]
		if !found {
			g.checks.Fail(check, "not in "+deadQueue+": "+body)
			continue
		}
		deaths, _ := delivery.Headers["x-death"].([]any)
		if len(deaths) == 0 {
			g.checks.Fail(check, "reached "+deadQueue+" without being rejected by the consumer: "+body)
			continue
		}
		first, _ := deaths[0].(amqp.Table)
		if first["reason"] != "rejected" || first["queue"] != incomingQueue || first["count"] != int64(1) {
			g.checks.Fail(check, fmt.Sprintf("x-death %v: expected one rejection from %s", first, incomingQueue))
		}
	}
	g.checks.Pass(check)
}

// New prepares a run in workDir on the network, running the program in image.
func New(workDir, image, network string) *intscenario.Handle {
	return intscenario.New(Scenario{}, workDir, image, network)
}

// Prompt is the scenario's task text.
func Prompt() string { return prompt }
