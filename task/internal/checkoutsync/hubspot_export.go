package checkoutsync

import (
	"net/http"
	"sort"
)

// HubSpotMock exposes the HubSpot CRM v3 contacts mock to other scenarios,
// which run it as a sidecar.
type HubSpotMock struct{ m *mocks }

// SeedContact is a contact present before a scenario starts.
type SeedContact = seedContact

// RecordedRequest is one request the mock received.
type RecordedRequest struct {
	Seq    int    `json:"seq"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
}

// NewHubSpotMock returns an empty contacts mock.
func NewHubSpotMock() *HubSpotMock {
	m := &mocks{}
	m.reset(nil, 0, 0)
	return &HubSpotMock{m: m}
}

// Handler serves the contacts API.
func (h *HubSpotMock) Handler() http.Handler { return trimTrailingSlash(h.m.hubspotHandler()) }

// Reset clears requests and contacts and seeds the given contacts.
func (h *HubSpotMock) Reset(seeds []SeedContact) { h.m.reset(seeds, 0, 0) }

// Requests lists the requests received, in order.
func (h *HubSpotMock) Requests() []RecordedRequest {
	var out []RecordedRequest
	for _, request := range h.m.snapshot(serviceHubSpot) {
		out = append(out, RecordedRequest{Seq: request.seq, Method: request.method, Path: request.path, Body: string(request.body)})
	}
	return out
}

// Contacts lists the stored contacts as the API renders them, by id.
func (h *HubSpotMock) Contacts() []map[string]any {
	h.m.mu.Lock()
	var contacts []contact
	for _, current := range h.m.contacts {
		contacts = append(contacts, h.m.copyContact(current))
	}
	h.m.mu.Unlock()
	sort.Slice(contacts, func(i, j int) bool { return contacts[i].id < contacts[j].id })
	out := make([]map[string]any, 0, len(contacts))
	for _, current := range contacts {
		out = append(out, contactJSON(current))
	}
	return out
}
