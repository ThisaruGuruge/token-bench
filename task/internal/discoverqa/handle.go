package discoverqa

import (
	"fmt"

	"token-bench/task/answer"
)

// Lookup returns the task with the given ID.
func Lookup(id string) (Definition, bool) {
	for _, definition := range Tasks {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

// Prompt is the complete text sent to the agent.
func (d Definition) Prompt() string {
	return fmt.Sprintf("%s\n\nQuestion: %s\n\nAnswer shape (answer.json):\n%s\n", Frame, d.Question, d.Shape)
}

// Handle is one run of a question task.
type Handle struct {
	definition Definition
	workDir    string
}

// New starts a run of the task in workDir.
func New(definition Definition, workDir string) *Handle {
	return &Handle{definition: definition, workDir: workDir}
}

func (h *Handle) Setup() (func() error, error) { return func() error { return nil }, nil }

func (h *Handle) Prompt() (string, error) { return h.definition.Prompt(), nil }

// Feedback is always empty: answer tasks are single shot.
func (h *Handle) Feedback() (string, error) { return "", nil }

func (h *Handle) Validation() (bool, error) {
	grade, err := h.Grade()
	return grade.Passed(), err
}

func (h *Handle) Grade() (answer.Grade, error) { return h.definition.GradeFile(h.workDir) }
