package leadrouting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"token-bench/task/answer"
)

// The checks, in grading order. A run passes only when all pass.
const (
	checkStarts        = "program builds and starts"
	checkRejects       = "invalid requests get 400 and call nothing"
	checkHotResponse   = "new HOT lead answered 201 with contact id and rating"
	checkHubSpotCreate = "new lead created in HubSpot with all properties"
	checkColdResponse  = "existing COLD lead answered 201 with its contact id"
	checkHubSpotUpdate = "existing lead updated, no duplicate contact"
	checkEnrichment    = "enrichment API called for both companies"
	checkModel         = "model asked for both leads"
	checkKafka         = "two records on topic leads with contactId, email, rating"
	checkMail          = "exactly one email, for the HOT lead, to sales"
)

var checks = []string{checkStarts, checkRejects, checkHotResponse, checkHubSpotCreate, checkColdResponse,
	checkHubSpotUpdate, checkEnrichment, checkModel, checkKafka, checkMail}

type hotLead struct{ email, first, last, company, message, industry string }

var (
	newLead      = hotLead{"bob@example.com", "Bob", "Baker", "Acme Corp", "Can you send pricing for 50 seats?", "Manufacturing"}
	existingLead = hotLead{seededEmail, "Alicia", "Archer", "Tiny Startup", "Just browsing your blog.", "Unknown"}
)

type grader struct {
	h       *Handle
	app     string
	base    string
	verdict map[string]string
}

// Grade runs the agent's program in a fresh container on the run network and
// checks every effect on the mocks and on Kafka. A machinery failure is
// returned as an *InfraError.
func (h *Handle) Grade() (answer.Grade, error) {
	g := &grader{h: h, verdict: map[string]string{}}
	grade, err := g.run()
	if err != nil {
		return answer.Grade{}, &InfraError{Err: err}
	}
	return grade, nil
}

func (g *grader) fail(check, reason string) {
	if _, seen := g.verdict[check]; !seen {
		g.verdict[check] = reason
	}
}

func (g *grader) pass(check string) {
	if _, seen := g.verdict[check]; !seen {
		g.verdict[check] = ""
	}
}

func (g *grader) result(problem string) answer.Grade {
	grade := answer.Grade{Present: problem == "", Problem: problem, Total: len(checks)}
	for _, check := range checks {
		reason, seen := g.verdict[check]
		if !seen {
			reason = "not reached"
		}
		field := answer.FieldGrade{Field: check, Correct: seen && reason == "", Got: reason, Expected: "pass"}
		if field.Correct {
			grade.Correct++
		}
		grade.Fields = append(grade.Fields, field)
	}
	return grade
}

func (g *grader) run() (answer.Grade, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	project, ok := projectDir(g.h.workDir)
	if !ok {
		g.fail(checkStarts, "no Ballerina.toml in the working directory or one level below")
		return g.result("no Ballerina project"), nil
	}
	if err := g.h.resetState(ctx); err != nil {
		return answer.Grade{}, err
	}
	g.app = g.h.name("app")
	g.h.containers = append(g.h.containers, g.app)
	if _, err := docker(ctx, "run", "-d", "--platform", platform, "--name", g.app, "--network", g.h.network,
		"-v", project+":/app", "-w", "/app", "-p", "127.0.0.1::"+appPort, "--entrypoint", "bal", g.h.image, "run"); err != nil {
		return answer.Grade{}, err
	}
	defer g.saveLogs()
	address, err := docker(ctx, "port", g.app, appPort)
	if err != nil {
		return answer.Grade{}, err
	}
	g.base = "http://" + strings.Split(address, "\n")[0]
	if reason := g.waitStarted(ctx); reason != "" {
		g.fail(checkStarts, reason)
		return g.result(""), nil
	}
	g.pass(checkStarts)
	time.Sleep(consumerSettle)
	g.checkRejections()
	hotID := g.post(newLead, "HOT", checkHotResponse)
	coldID := g.post(existingLead, "COLD", checkColdResponse)
	g.checkHubSpot(hotID, coldID)
	g.checkEnrichmentAndModel()
	g.checkKafka(ctx, hotID, coldID)
	g.checkMail(hotID)
	return g.result(""), nil
}

func (g *grader) saveLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logs, _ := docker(ctx, "logs", g.app)
	_ = os.WriteFile(filepath.Join(g.h.workDir, "grader-app.log"), []byte(logs+"\n"), 0o644)
	_, _ = docker(ctx, "rm", "-f", g.app)
}

// waitStarted waits for the program to answer on its port, or to exit.
func (g *grader) waitStarted(ctx context.Context) string {
	deadline := time.Now().Add(startTimeout)
	client := http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if response, err := client.Get(g.base + "/leads"); err == nil {
			response.Body.Close()
			return ""
		}
		if state, _ := docker(ctx, "inspect", "-f", "{{.State.Running}}", g.app); state == "false" {
			logs, _ := docker(ctx, "logs", "--tail", "15", g.app)
			return "the program exited: " + oneLine(logs)
		}
		time.Sleep(time.Second)
	}
	return "port 8090 not answering after " + startTimeout.String()
}

func oneLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 300 {
		text = text[len(text)-300:]
	}
	return text
}

func (g *grader) send(body string) (int, map[string]any, string) {
	client := http.Client{Timeout: requestTimeout}
	response, err := client.Post(g.base+"/leads", "application/json", strings.NewReader(body))
	if err != nil {
		return 0, nil, err.Error()
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded, string(raw)
}

func (g *grader) checkRejections() {
	for _, body := range []string{`{"email": "x@example.com", "firstName": "X"`, `{"email": "", "firstName": "X", "lastName": "Y", "company": "Initech", "message": "pricing"}`,
		`{"firstName": "X", "lastName": "Y", "company": "Initech", "message": "pricing"}`} {
		if status, _, raw := g.send(body); status != http.StatusBadRequest {
			g.fail(checkRejects, fmt.Sprintf("%s answered %d %s", body, status, oneLine(raw)))
		}
	}
	for _, service := range mockServices {
		if requests := g.requests(service); len(requests) > 0 {
			g.fail(checkRejects, fmt.Sprintf("%s was called %d times for rejected requests", service, len(requests)))
		}
	}
	g.pass(checkRejects)
}

func (g *grader) post(lead hotLead, rating, check string) string {
	body, _ := json.Marshal(map[string]string{"email": lead.email, "firstName": lead.first, "lastName": lead.last, "company": lead.company, "message": lead.message})
	status, decoded, raw := g.send(string(body))
	if status != http.StatusCreated {
		g.fail(check, fmt.Sprintf("answered %d %s", status, oneLine(raw)))
		return ""
	}
	id, _ := decoded["contactId"].(string)
	if id == "" || decoded["rating"] != rating {
		g.fail(check, "body "+oneLine(raw))
		return id
	}
	if lead.email == seededEmail && id != seededID {
		g.fail(check, "contactId "+id+", expected the existing contact "+seededID)
	}
	g.pass(check)
	return id
}

func (g *grader) requests(service string) []Request {
	response, err := http.Get(g.h.controls[service] + "/__requests")
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var requests []Request
	_ = json.NewDecoder(response.Body).Decode(&requests)
	return requests
}

func (g *grader) contacts() []map[string]any {
	response, err := http.Get(g.h.controls["hubspot"] + "/__contacts")
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var contacts []map[string]any
	_ = json.NewDecoder(response.Body).Decode(&contacts)
	return contacts
}

func properties(contact map[string]any) map[string]any {
	values, _ := contact["properties"].(map[string]any)
	return values
}

func (g *grader) checkHubSpot(hotID, coldID string) {
	contacts := g.contacts()
	if len(contacts) != 2 {
		g.fail(checkHubSpotUpdate, fmt.Sprintf("%d contacts stored, expected 2 (one seeded, one new)", len(contacts)))
	}
	creates := 0
	for _, request := range g.requests("hubspot") {
		if request.Method == http.MethodPost && strings.TrimRight(request.Path, "/") == "/crm/v3/objects/contacts" {
			creates++
		}
	}
	if creates != 1 {
		g.fail(checkHubSpotCreate, fmt.Sprintf("%d contact creations, expected 1", creates))
	}
	want := map[string]map[string]string{
		hotID:  {"email": newLead.email, "firstname": newLead.first, "lastname": newLead.last, "company": newLead.company, "industry": newLead.industry, "lead_rating": "HOT"},
		coldID: {"email": existingLead.email, "firstname": existingLead.first, "lastname": existingLead.last, "company": existingLead.company, "industry": existingLead.industry, "lead_rating": "COLD"},
	}
	for id, expected := range want {
		check := checkHubSpotCreate
		if id == coldID {
			check = checkHubSpotUpdate
		}
		var found map[string]any
		for _, contact := range contacts {
			if contact["id"] == id {
				found = properties(contact)
			}
		}
		if found == nil {
			g.fail(check, "no stored contact with id "+id)
			continue
		}
		for key, value := range expected {
			if fmt.Sprint(found[key]) != value {
				g.fail(check, fmt.Sprintf("contact %s: %s = %v, expected %q", id, key, found[key], value))
			}
		}
	}
	g.pass(checkHubSpotCreate)
	g.pass(checkHubSpotUpdate)
}

func (g *grader) checkEnrichmentAndModel() {
	seen := map[string]bool{}
	for _, request := range g.requests("enrich") {
		seen[request.Path] = true
	}
	for _, company := range []string{newLead.company, existingLead.company} {
		if !seen["/companies/"+company] {
			g.fail(checkEnrichment, "no GET /companies/"+company)
		}
	}
	g.pass(checkEnrichment)
	asked := map[string]bool{}
	for _, request := range g.requests("openai") {
		for _, lead := range []hotLead{newLead, existingLead} {
			if strings.Contains(request.Body, lead.message) {
				asked[lead.email] = true
			}
		}
	}
	if len(asked) != 2 {
		g.fail(checkModel, fmt.Sprintf("the model saw %d of the 2 lead messages", len(asked)))
	}
	g.pass(checkModel)
}

func (g *grader) checkKafka(ctx context.Context, hotID, coldID string) {
	output, err := docker(ctx, "exec", g.h.name("kafka"), kafkaBin+"kafka-console-consumer.sh", "--bootstrap-server", "localhost:9092",
		"--topic", topic, "--from-beginning", "--timeout-ms", "8000")
	if err != nil && !strings.Contains(err.Error(), "TimeoutException") && output == "" {
		g.fail(checkKafka, "cannot read the topic: "+err.Error())
		return
	}
	var records []map[string]any
	for _, line := range strings.Split(output, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &record) == nil && record != nil {
			records = append(records, record)
		}
	}
	if len(records) != 2 {
		g.fail(checkKafka, fmt.Sprintf("%d JSON records on %s, expected 2: %s", len(records), topic, oneLine(output)))
		return
	}
	want := map[string]string{hotID + "|" + newLead.email + "|HOT": "", coldID + "|" + existingLead.email + "|COLD": ""}
	for _, record := range records {
		key := fmt.Sprint(record["contactId"]) + "|" + fmt.Sprint(record["email"]) + "|" + fmt.Sprint(record["rating"])
		if _, ok := want[key]; !ok {
			g.fail(checkKafka, "unexpected record "+oneLine(fmt.Sprint(record)))
		}
	}
	g.pass(checkKafka)
}

func (g *grader) checkMail(hotID string) {
	deadline := time.Now().Add(mailWait)
	var sends []Request
	for time.Now().Before(deadline) {
		sends = g.sends()
		if len(sends) >= 1 {
			break
		}
		time.Sleep(time.Second)
	}
	time.Sleep(5 * time.Second)
	sends = g.sends()
	if len(sends) != 1 {
		g.fail(checkMail, fmt.Sprintf("%d emails sent, expected 1", len(sends)))
		return
	}
	message := sends[0].Decoded
	lower := strings.ToLower(message)
	switch {
	case !strings.Contains(lower, "to: "+salesAddress) && !strings.Contains(lower, "to:"+salesAddress) && !strings.Contains(lower, "<"+salesAddress+">"):
		g.fail(checkMail, "not addressed to "+salesAddress+": "+oneLine(message))
	case !strings.Contains(message, "Hot lead: "+newLead.email):
		g.fail(checkMail, "subject is not 'Hot lead: "+newLead.email+"': "+oneLine(message))
	case hotID == "" || !strings.Contains(message, hotID):
		g.fail(checkMail, "body does not contain contact id "+hotID)
	case !strings.Contains(sends[0].Path, "/users/me/"):
		g.fail(checkMail, "not sent as user me: "+sends[0].Path)
	}
	g.pass(checkMail)
}

func (g *grader) sends() []Request {
	var sends []Request
	for _, request := range g.requests("gmail") {
		if request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/messages/send") {
			sends = append(sends, request)
		}
	}
	return sends
}
