package task

import (
	"token-bench/task/internal/eventbridge"
	"token-bench/task/internal/graphservice"
	"token-bench/task/internal/ordersync"
)

func newIntegration(name, prompt string, start func(workDir, image, network string) TaskHandle) Task {
	return Task{
		Name:                 name,
		Kind:                 KindAnswer,
		Network:              true,
		RequirementsTemplate: prompt,
		InitFn: func(resources AllocResources) TaskHandle {
			return start(resources.WorkDir, resources.Image, resources.Network)
		},
	}
}

// NewOrderSync is the order-sync integration scenario of the grammar benchmark.
func NewOrderSync() Task {
	return newIntegration(ordersync.Name, ordersync.Prompt(), func(w, i, n string) TaskHandle { return ordersync.New(w, i, n) })
}

// NewEventBridge is the event-bridge integration scenario of the grammar benchmark.
func NewEventBridge() Task {
	return newIntegration(eventbridge.Name, eventbridge.Prompt(), func(w, i, n string) TaskHandle { return eventbridge.New(w, i, n) })
}

// NewGraphService is the graph-service integration scenario of the grammar benchmark.
func NewGraphService() Task {
	return newIntegration(graphservice.Name, graphservice.Prompt(), func(w, i, n string) TaskHandle { return graphservice.New(w, i, n) })
}
