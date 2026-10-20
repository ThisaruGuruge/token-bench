package discoverqa

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"token-bench/task/answer"
)

// Grade reads and grades an answer document.
func (d Definition) Grade(content []byte) answer.Grade {
	grade := answer.Grade{Total: len(d.Fields)}
	var document map[string]any
	if err := json.Unmarshal(content, &document); err != nil || document == nil {
		grade.Problem = "answer.json is not a JSON object"
		for _, field := range d.Fields {
			grade.Fields = append(grade.Fields, answer.FieldGrade{Field: field.Key, Expected: field.expected()})
		}
		return grade
	}
	grade.Present = true
	for _, field := range d.Fields {
		value, ok := document[field.Key]
		verdict := answer.FieldGrade{Field: field.Key, Expected: field.expected()}
		if ok {
			verdict.Got = compact(value)
			verdict.Correct = field.matches(value)
		}
		if verdict.Correct {
			grade.Correct++
		}
		grade.Fields = append(grade.Fields, verdict)
	}
	return grade
}

// GradeFile grades workDir/answer.json; a missing file is a failed answer, not an error.
func (d Definition) GradeFile(workDir string) (answer.Grade, error) {
	content, err := os.ReadFile(filepath.Join(workDir, answer.FileName))
	if errors.Is(err, os.ErrNotExist) {
		grade := d.Grade(nil)
		grade.Problem = "answer.json was not written"
		return grade, nil
	}
	if err != nil {
		return answer.Grade{}, err
	}
	return d.Grade(content), nil
}

func compact(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func (f Field) expected() string {
	if f.Kind == kindParams {
		parts := make([]string, 0, len(f.Params))
		for _, param := range f.Params {
			part := param.Name + " " + param.Type
			if param.Default != "" {
				part += " = " + param.Default
			}
			parts = append(parts, part)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	switch f.Kind {
	case kindNames:
		return "{" + strings.Join(f.Want, ", ") + "}"
	case kindTypes, kindChain:
		return "[" + strings.Join(f.Want, ", ") + "]"
	}
	return strings.Join(f.Want, " or ")
}

func (f Field) matches(value any) bool {
	switch f.Kind {
	case kindNames:
		got, ok := stringList(value)
		return ok && sameSet(mapAll(got, normalizeName), mapAll(f.Want, normalizeName))
	case kindChain:
		got, ok := stringList(value)
		return ok && sameList(mapAll(got, normalizeName), mapAll(f.Want, normalizeName))
	case kindTypes:
		got, ok := stringList(value)
		return ok && sameList(mapAll(got, normalizeType), mapAll(f.Want, normalizeType))
	case kindParams:
		return f.paramsMatch(value)
	case kindInt:
		text := scalar(value)
		number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		want, _ := strconv.ParseFloat(f.Want[0], 64)
		return err == nil && number == want
	}
	text := scalar(value)
	if text == "" {
		return false
	}
	normalize := normalizerFor(f.Kind)
	for _, want := range f.Want {
		if normalize(text) == normalize(want) {
			return true
		}
	}
	return false
}

func normalizerFor(kind string) func(string) string {
	switch kind {
	case kindType:
		return normalizeType
	case kindPath:
		return normalizePath
	case kindValue:
		return normalizeValue
	case kindLibrary:
		return normalizeLibrary
	}
	return normalizeName
}

func (f Field) paramsMatch(value any) bool {
	list, ok := value.([]any)
	if !ok || len(list) != len(f.Params) {
		return false
	}
	for index, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			return false
		}
		want := f.Params[index]
		if normalizeName(scalar(object["name"])) != normalizeName(want.Name) {
			return false
		}
		if normalizeType(scalar(object["type"])) != normalizeType(want.Type) {
			return false
		}
		if want.Default != "" && normalizeValue(scalar(object["default"])) != normalizeValue(want.Default) {
			return false
		}
	}
	return true
}

// scalar renders a JSON scalar as text; numbers keep their shortest form.
func scalar(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	return ""
}

func stringList(value any) ([]string, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

func mapAll(values []string, transform func(string) string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = transform(value)
	}
	return out
}

func sameList(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameSet(left, right []string) bool {
	left, right = dedupe(left), dedupe(right)
	return sameList(left, right)
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
