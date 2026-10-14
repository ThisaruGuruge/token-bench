package discoverqa

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNormalizeType(t *testing.T) {
	cases := map[string]string{
		"kafka:Error?":                     "()|Error",
		"Error|()":                         "()|Error",
		"TopicPartition[] | kafka:Error":   "Error|TopicPartition[]",
		"'type|error":                      "error|type",
		"sql:ParameterizedQuery[]":         "ParameterizedQuery[]",
		"map<string|string[]>?":            "()|map<string|string[]>",
		"*s3:PutObjectConfig":              "PutObjectConfig",
		"BaseGist[]|error?":                "()|BaseGist[]|error",
		"http:targetType|http:ClientError": "ClientError|targetType",
	}
	for input, want := range cases {
		if got := normalizeType(input); got != want {
			t.Errorf("normalizeType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	for _, input := range []string{"/repos/:owner/:repo/issues", "repos/[string owner]/[repo]/issues", "repos/{owner}/{repo}/issues"} {
		if got := normalizePath(input); got != "repos/:owner/:repo/issues" {
			t.Errorf("normalizePath(%q) = %q", input, got)
		}
	}
	if got := normalizePath("gists/'public"); got != "gists/public" {
		t.Errorf("escape not dropped: %q", got)
	}
}

func TestNormalizeValue(t *testing.T) {
	cases := map[string]string{`"localhost"`: "localhost", "30.0": "30", "-1": "-1", "http:HTTP_2_0": "HTTP_2_0", "60": "60"}
	for input, want := range cases {
		if got := normalizeValue(input); got != want {
			t.Errorf("normalizeValue(%q) = %q, want %q", input, got, want)
		}
	}
}

// expectedDocument builds a correct answer from a task's own expectations.
func expectedDocument(definition Definition) map[string]any {
	document := map[string]any{}
	for _, field := range definition.Fields {
		switch field.Kind {
		case kindNames, kindTypes:
			list := make([]any, len(field.Want))
			for index, value := range field.Want {
				list[index] = value
			}
			document[field.Key] = list
		case kindParams:
			var params []any
			for _, param := range field.Params {
				entry := map[string]any{"name": param.Name, "type": param.Type}
				if param.Default != "" {
					entry["default"] = param.Default
				}
				params = append(params, entry)
			}
			document[field.Key] = params
		case kindInt:
			document[field.Key] = 199
		default:
			document[field.Key] = field.Want[0]
		}
	}
	return document
}

func TestEveryTaskAcceptsItsExpectedAnswerAndRejectsAnEmptyOne(t *testing.T) {
	if len(Tasks) != 18 {
		t.Fatalf("want 18 tasks, got %d", len(Tasks))
	}
	for _, definition := range Tasks {
		content, _ := json.Marshal(expectedDocument(definition))
		grade := definition.Grade(content)
		if !grade.Passed() {
			t.Errorf("%s: its own expected answer fails: %+v", definition.ID, grade)
		}
		if empty := definition.Grade([]byte(`{}`)); empty.Passed() || empty.Correct != 0 || !empty.Present {
			t.Errorf("%s: empty answer graded %+v", definition.ID, empty)
		}
		if broken := definition.Grade([]byte(`not json`)); broken.Present || broken.Passed() {
			t.Errorf("%s: malformed answer graded %+v", definition.ID, broken)
		}
		if len(definition.FirstCommands["A"]) == 0 || len(definition.FirstCommands["B"]) == 0 {
			t.Errorf("%s: needs on-target first commands for both grammars", definition.ID)
		}
	}
}

func TestEquivalentSpellingsPass(t *testing.T) {
	d03, _ := Lookup("D03")
	grade := d03.Grade([]byte(`{"parameters":[{"name":"topic","type":"string"},{"name":"duration","type":"decimal","default":"-1.0"}],"returnType":"kafka:TopicPartition[]|kafka:Error"}`))
	if !grade.Passed() {
		t.Errorf("D03 equivalent spelling failed: %+v", grade)
	}
	d08, _ := Lookup("D08")
	grade = d08.Grade([]byte(`{"accessor":"post","path":"/repos/[string owner]/[string repo]/issues","payloadType":"github:RepoIssuesBody","requiredFields":["title"],"returnType":"github:Issue|error"}`))
	if !grade.Passed() {
		t.Errorf("D08 equivalent spelling failed: %+v", grade)
	}
	d11, _ := Lookup("D11")
	if !d11.Grade([]byte(`{"operationCount":"199","operation":"fetchCall","returnType":"Call|error"}`)).Passed() {
		t.Error("D11 count as a string should pass")
	}
}

func TestWrongAnswersFailTheRightField(t *testing.T) {
	d09, _ := Lookup("D09")
	grade := d09.Grade([]byte(`{"accessor":"get","path":"gists/public","returnType":"BaseGist[]|error"}`))
	if grade.Passed() || grade.Correct != 2 || grade.Fields[2].Correct {
		t.Errorf("D09 missing the optional return should fail only returnType: %+v", grade)
	}
	d03, _ := Lookup("D03")
	if d03.Grade([]byte(`{"parameters":[{"name":"topic","type":"string"},{"name":"duration","type":"decimal"}],"returnType":"TopicPartition[]|Error"}`)).Passed() {
		t.Error("D03 without the default should fail")
	}
	d01, _ := Lookup("D01")
	if d01.Grade([]byte(`{"operations":["close","flush","send"]}`)).Passed() {
		t.Error("D01 with missing operations should fail")
	}
}

func TestGradeFileMissing(t *testing.T) {
	d01, _ := Lookup("D01")
	grade, err := d01.GradeFile(t.TempDir())
	if err != nil || grade.Present || grade.Problem != "answer.json was not written" {
		t.Fatalf("missing answer: %+v %v", grade, err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "answer.json"), []byte(`{"operations":["close","flush","getTopicPartitions","send","sendWithMetadata"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if grade, _ := d01.GradeFile(dir); !grade.Passed() {
		t.Fatalf("written answer: %+v", grade)
	}
}

// bannedWords are grammar tokens of only one grammar (design §4.1).
var bannedWords = regexp.MustCompile(`(?i)\b(methods?|functions?|resources?|packages?|funcs|buckets?)\b|(^|\s)-p\b`)

func TestPromptsPassTheWordingLint(t *testing.T) {
	for _, definition := range Tasks {
		if match := bannedWords.FindString(definition.Prompt()); match != "" {
			t.Errorf("%s: prompt uses grammar token %q", definition.ID, strings.TrimSpace(match))
		}
	}
}

func TestPilotIDsExist(t *testing.T) {
	for _, id := range PilotIDs {
		if _, ok := Lookup(id); !ok {
			t.Errorf("pilot task %s missing", id)
		}
	}
}
