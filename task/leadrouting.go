package task

import "token-bench/task/internal/leadrouting"

// LeadRoutingName is the integration scenario of the grammar benchmark.
const LeadRoutingName = "lead-routing"

// NewLeadRouting is the integration scenario: an HTTP service using an
// enrichment API, an AI model, HubSpot, Kafka and Gmail, graded by running the
// agent's program against mock sidecars and a real Kafka broker on a per-run
// docker network. It needs a container arm with an integration image.
func NewLeadRouting() Task {
	return Task{
		Name:                 LeadRoutingName,
		Kind:                 KindAnswer,
		Network:              true,
		RequirementsTemplate: leadrouting.Prompt(),
		InitFn: func(resources AllocResources) TaskHandle {
			return leadrouting.New(resources.WorkDir, resources.Image, resources.Network)
		},
	}
}
