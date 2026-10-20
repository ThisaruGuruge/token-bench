package task

import "token-bench/task/internal/discoverqa"

// DiscoverQA returns the discovery-only question tasks (scenarios dq-D01 ..
// dq-D18) of the grammar benchmark.
func DiscoverQA() []Task {
	tasks := make([]Task, 0, len(discoverqa.Tasks))
	for _, definition := range discoverqa.Tasks {
		tasks = append(tasks, newDiscoverQA(definition))
	}
	return tasks
}

func newDiscoverQA(definition discoverqa.Definition) Task {
	return Task{
		Name:                 discoverqa.ScenarioName(definition.ID),
		Kind:                 KindAnswer,
		RequirementsTemplate: definition.Prompt(),
		FirstCommands:        definition.FirstCommands,
		InitFn: func(resources AllocResources) TaskHandle {
			return discoverqa.New(definition, resources.WorkDir)
		},
	}
}

// DiscoverQAPilot lists the scenario names of the 6-task pilot subset.
func DiscoverQAPilot() []string {
	names := make([]string, 0, len(discoverqa.PilotIDs))
	for _, id := range discoverqa.PilotIDs {
		names = append(names, discoverqa.ScenarioName(id))
	}
	return names
}

// ServiceTasks returns the build-a-service scenarios.
func ServiceTasks() []Task {
	return []Task{
		NewCheckoutSync(), NewClaimCheck(), NewContentBasedRouter(), NewContentEnricher(),
		NewDeadLetterChannel(), NewMultiConsumer(), NewScatterGather(), NewLeadRouting(),
	}
}

// ByName returns the scenario with the given name.
func ByName(name string) (Task, bool) {
	for _, candidate := range append(ServiceTasks(), DiscoverQA()...) {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return Task{}, false
}

// Names lists every scenario name.
func Names() []string {
	var names []string
	for _, candidate := range append(ServiceTasks(), DiscoverQA()...) {
		names = append(names, candidate.Name)
	}
	return names
}
