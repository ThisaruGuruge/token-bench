// Command check grades a project directory against an integration scenario at
// no cost (no agent): it sets up the network, the sidecars and the mocks, runs
// the project in the given integration image and prints the grade. The project
// is copied first, so repeated runs start from the same files.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"token-bench/task/answer"
	"token-bench/task/internal/scenarios"
)

func main() {
	scenario := flag.String("scenario", "", "scenario name ("+strings.Join(scenarios.Names(), ", ")+")")
	image := flag.String("image", "token-bench-discover:GA-int", "integration image")
	project := flag.String("project", "", "project directory to grade")
	scratch := flag.String("scratch", "", "directory for the graded copy (must be mountable)")
	keep := flag.Bool("keep", false, "keep the graded copy")
	flag.Parse()
	work, err := os.MkdirTemp(*scratch, "scenario-check-*")
	if err != nil {
		fail(err)
	}
	if !*keep {
		defer os.RemoveAll(work)
	}
	if output, err := exec.Command("cp", "-R", *project+"/.", work).CombinedOutput(); err != nil {
		fail(fmt.Errorf("copy: %v %s", err, output))
	}
	_ = exec.Command("chmod", "-R", "a+rwX", work).Run()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	handle, ok := scenarios.New(*scenario, work, *image, "tb-net-check-"+hex.EncodeToString(suffix))
	if !ok {
		fail(fmt.Errorf("unknown scenario %q", *scenario))
	}
	cleanup, err := handle.Setup()
	var grade answer.Grade
	if err == nil {
		grade, err = handle.Grade()
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		fmt.Fprintln(os.Stderr, "cleanup:", cleanupErr)
	}
	if err != nil {
		fail(err)
	}
	encoded, _ := json.MarshalIndent(grade, "", "  ")
	fmt.Println(string(encoded))
	if logs, err := os.ReadFile(filepath.Join(work, "grader-app.log")); err == nil && !grade.Passed() {
		tail := string(logs)
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		fmt.Println("--- app log (tail) ---\n" + tail)
	}
	if *keep {
		fmt.Println("kept:", work)
	}
	if !grade.Passed() {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
