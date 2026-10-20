// Command mockserver runs one mock of the lead-routing scenario as a sidecar:
// -service hubspot|gmail|openai|enrich. Every mock also serves GET /__requests
// (the recorded requests) and POST /__reset; hubspot also GET /__contacts and
// POST /__reset with a JSON list of seed contacts.
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
	"token-bench/task/internal/leadrouting"
)

func main() {
	service := flag.String("service", "", "hubspot, gmail, openai or enrich")
	address := flag.String("listen", ":8080", "listen address")
	flag.Parse()
	control := http.NewServeMux()
	var api http.Handler
	switch *service {
	case "hubspot":
		hubspot := checkoutsync.NewHubSpotMock()
		api = hubspot.Handler()
		control.HandleFunc("GET /__requests", func(writer http.ResponseWriter, _ *http.Request) { respond(writer, hubspot.Requests()) })
		control.HandleFunc("GET /__contacts", func(writer http.ResponseWriter, _ *http.Request) { respond(writer, hubspot.Contacts()) })
		control.HandleFunc("POST /__reset", func(writer http.ResponseWriter, request *http.Request) {
			var seeds []checkoutsync.SeedContact
			body, _ := io.ReadAll(request.Body)
			if len(strings.TrimSpace(string(body))) > 0 {
				if err := json.Unmarshal(body, &seeds); err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)
					return
				}
			}
			hubspot.Reset(seeds)
			respond(writer, map[string]any{"seeded": len(seeds)})
		})
	case "gmail", "openai", "enrich":
		recorder := &leadrouting.Recorder{}
		api = map[string]func(*leadrouting.Recorder) http.Handler{
			"gmail": leadrouting.GmailHandler, "openai": leadrouting.OpenAIHandler, "enrich": leadrouting.EnrichHandler,
		}[*service](recorder)
		control.HandleFunc("GET /__requests", func(writer http.ResponseWriter, _ *http.Request) { respond(writer, recorder.Requests()) })
		control.HandleFunc("POST /__reset", func(writer http.ResponseWriter, _ *http.Request) { recorder.Reset(); respond(writer, map[string]any{}) })
	default:
		log.Fatalf("unknown -service %q", *service)
	}
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
