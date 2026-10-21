// Command regrade-answers re-grades every question-task result below the given
// directories from the answer.json the agent left in its run directory, with
// the current normaliser, at no cost. The previous grade is kept in the result
// (answerOriginal) when it differs; the strict pass/fail is not changed unless
// the new grade says otherwise, and then it is reported.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"token-bench/task"
)

func main() {
	var regraded, changed, flipped int
	for _, root := range os.Args[1:] {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || entry.Name() != "result.json" || strings.Contains(path, "/.") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			var value map[string]any
			if json.Unmarshal(raw, &value) != nil {
				return nil
			}
			scenario, _ := value["task"].(string)
			content, readErr := os.ReadFile(filepath.Join(filepath.Dir(path), "answer.json"))
			if readErr != nil {
				content = nil
			}
			grade, ok := task.GradeAnswer(scenario, content)
			if !ok {
				return nil
			}
			regraded++
			if old, _ := json.Marshal(value["answer"]); string(old) != mustJSON(grade) {
				changed++
				if _, kept := value["answerOriginal"]; !kept {
					value["answerOriginal"] = value["answer"]
				}
			}
			value["answer"] = json.RawMessage(mustJSON(grade))
			if passed := grade.Passed(); passed != value["passed"] {
				flipped++
				fmt.Printf("pass/fail differs: %s (was %v, now %v)\n", path, value["passed"], passed)
			}
			encoded, _ := json.MarshalIndent(value, "", "  ")
			_ = os.WriteFile(path, append(encoded, '\n'), 0o644)
			return nil
		})
	}
	fmt.Printf("regraded %d question runs; %d grades changed; %d pass/fail verdicts differ (left unchanged)\n", regraded, changed, flipped)
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
