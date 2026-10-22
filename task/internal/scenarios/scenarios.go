// Package scenarios is the registry of the integration scenarios built on
// intscenario.
package scenarios

import (
	"token-bench/task/internal/eventbridge"
	"token-bench/task/internal/graphservice"
	"token-bench/task/internal/intscenario"
	"token-bench/task/internal/ordersync"
)

// New returns a run of the named scenario, or false for an unknown name.
func New(name, workDir, image, network string) (*intscenario.Handle, bool) {
	switch name {
	case ordersync.Name:
		return ordersync.New(workDir, image, network), true
	case eventbridge.Name:
		return eventbridge.New(workDir, image, network), true
	case graphservice.Name:
		return graphservice.New(workDir, image, network), true
	}
	return nil, false
}

// Names lists the registered scenarios.
func Names() []string { return []string{ordersync.Name, eventbridge.Name, graphservice.Name} }
