// Command mockserver runs one mock API of the integration scenarios as a
// sidecar: -service <name>. Every mock also serves GET /__requests (what it
// received), POST /__reset and GET /__health.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"token-bench/task/internal/checkoutsync"
	"token-bench/task/internal/intscenario"
	"token-bench/task/internal/leadrouting"
)

func main() {
	service := flag.String("service", "", "mock name")
	address := flag.String("listen", ":8080", "listen address")
	flag.Parse()
	control := http.NewServeMux()
	var api http.Handler
	recorder := &intscenario.Recorder{}
	listRequests := func() any { return recorder.Requests() }
	reset := func(io.Reader) { recorder.Reset() }
	switch *service {
	case "hubspot":
		hubspot := checkoutsync.NewHubSpotMock()
		api = hubspot.Handler()
		listRequests = func() any { return hubspot.Requests() }
		control.HandleFunc("GET /__contacts", func(writer http.ResponseWriter, _ *http.Request) { respond(writer, hubspot.Contacts()) })
		reset = func(body io.Reader) {
			var seeds []checkoutsync.SeedContact
			raw, _ := io.ReadAll(body)
			if len(strings.TrimSpace(string(raw))) > 0 {
				_ = json.Unmarshal(raw, &seeds)
			}
			hubspot.Reset(seeds)
		}
	case "gmail", "openai", "enrich":
		old := &leadrouting.Recorder{}
		api = map[string]func(*leadrouting.Recorder) http.Handler{
			"gmail": leadrouting.GmailHandler, "openai": leadrouting.OpenAIHandler, "enrich": leadrouting.EnrichHandler,
		}[*service](old)
		listRequests = func() any { return old.Requests() }
		reset = func(io.Reader) { old.Reset() }
	case "pricing":
		flakiness := &intscenario.Flakiness{}
		api = intscenario.PricingHandler(recorder, flakiness)
		reset = func(io.Reader) { recorder.Reset(); flakiness.Reset() }
	case "fraud":
		api = intscenario.FraudHandler(recorder)
	case "mistral":
		api = intscenario.OpenAIShapedHandler(recorder, intscenario.OrderPriority)
	case "ollama":
		api = intscenario.OllamaHandler(recorder, intscenario.ProductDescription)
	case "customers":
		api = intscenario.CustomersHandler(recorder)
	case "pagerduty":
		api = intscenario.PagerDutyHandler(recorder)
	case "mandrill":
		api = intscenario.MandrillHandler(recorder)
	default:
		log.Fatalf("unknown -service %q", *service)
	}
	control.HandleFunc("GET /__requests", func(writer http.ResponseWriter, _ *http.Request) { respond(writer, listRequests()) })
	control.HandleFunc("POST /__reset", func(writer http.ResponseWriter, request *http.Request) {
		reset(request.Body)
		respond(writer, map[string]any{})
	})
	control.HandleFunc("GET /__health", func(writer http.ResponseWriter, _ *http.Request) { respond(writer, map[string]any{"ok": true}) })
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/__") {
			control.ServeHTTP(writer, request)
			return
		}
		api.ServeHTTP(writer, request)
	})
	server := &http.Server{Addr: *address, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func respond(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
