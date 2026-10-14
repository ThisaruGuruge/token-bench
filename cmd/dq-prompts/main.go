// Command dq-prompts prints the full prompt of every discover question task,
// for the wording lint and for review.
package main

import (
	"fmt"

	"token-bench/task"
)

func main() {
	for _, definition := range task.DiscoverQA() {
		fmt.Printf("=== %s\n%s\n", definition.Name, definition.RequirementsTemplate)
	}
}
