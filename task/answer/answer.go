// Package answer holds the grading types shared by answer-kind tasks, the
// harness and the persisted result.
package answer

// FileName is the file, in the agent's working directory, an answer task reads.
const FileName = "answer.json"

// FieldGrade is the verdict on one field of an answer.
type FieldGrade struct {
	Field    string `json:"field"`
	Correct  bool   `json:"correct"`
	Got      string `json:"got,omitempty"`
	Expected string `json:"expected"`
}

// Grade is the verdict on a whole answer. The run passes only when every field
// is correct; Correct/Total is the partial score.
type Grade struct {
	// Present is false when answer.json is missing or not a JSON object.
	Present bool         `json:"present"`
	Problem string       `json:"problem,omitempty"`
	Correct int          `json:"correct"`
	Total   int          `json:"total"`
	Fields  []FieldGrade `json:"fields"`
}

// Passed reports whether every field is correct.
func (g Grade) Passed() bool { return g.Present && g.Total > 0 && g.Correct == g.Total }

// Grader is implemented by task handles that can explain their verdict.
type Grader interface {
	Grade() (Grade, error)
}
