// Command check grades a lead-routing project directory ($0, no agent): it sets
// up the run network, Kafka and the mocks, runs the project in the given
// integration image and prints the grade. -copy grades a fresh copy of the
// directory, so repeated runs start from the same files.
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

	"token-bench/task/answer"
	"token-bench/task/internal/leadrouting"
)

func main() {
	image := flag.String("image", "token-bench-discover:GA-int", "integration image")
	project := flag.String("project", "", "project directory to grade")
	scratch := flag.String("scratch", "", "directory for the graded copy (must be mountable)")
	flag.Parse()
	work, err := os.MkdirTemp(*scratch, "leadrouting-check-*")
	if err != nil {
		fail(err)
	}
	if output, err := exec.Command("cp", "-R", *project+"/.", work).CombinedOutput(); err != nil {
		fail(fmt.Errorf("copy: %v %s", err, output))
	}
	_ = exec.Command("chmod", "-R", "a+rwX", work).Run()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	handle := leadrouting.New(work, *image, "tb-net-check-"+hex.EncodeToString(suffix))
	cleanup, err := handle.Setup()
	if err == nil {
		var grade answer.Grade
		grade, err = handle.Grade()
		if err == nil {
			report(grade, work)
		}
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		fmt.Fprintln(os.Stderr, "cleanup:", cleanupErr)
	}
	if err != nil {
		fail(err)
	}
	if !passed {
		os.Exit(1)
	}
}

var passed bool

func report(grade answer.Grade, work string) {
	passed = grade.Passed()
	encoded, _ := json.MarshalIndent(grade, "", "  ")
	fmt.Println(string(encoded))
	fmt.Println("work dir:", work, "app log:", filepath.Join(work, "grader-app.log"))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
