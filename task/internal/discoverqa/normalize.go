package discoverqa

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	modulePrefix = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_.]*:`)
	whitespace   = regexp.MustCompile(`\s+`)
	pathParam    = regexp.MustCompile(`^(?:\[(?:[A-Za-z_][A-Za-z0-9_]*\s+)?([A-Za-z_][A-Za-z0-9_]*)\]|\{([A-Za-z_][A-Za-z0-9_]*)\}|:([A-Za-z_][A-Za-z0-9_]*))$`)
)

// normalizeType canonicalises a Ballerina type as the grader compares it:
// module prefixes dropped, whitespace removed, a quoted identifier unquoted,
// an included-record `*` dropped, `T?` read as `T|()`, and top-level union
// members sorted.
func normalizeType(value string) string {
	text := strings.TrimSpace(value)
	text = modulePrefix.ReplaceAllString(text, "")
	text = whitespace.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "'", "")
	text = strings.TrimPrefix(text, "*")
	var expanded []string
	for _, member := range splitTopLevel(text, '|') {
		if optional, found := strings.CutSuffix(member, "?"); found {
			expanded = append(expanded, optional, "()")
			continue
		}
		expanded = append(expanded, member)
	}
	unique := map[string]bool{}
	var out []string
	for _, member := range expanded {
		if member == "" || unique[member] {
			continue
		}
		unique[member] = true
		out = append(out, member)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

// splitTopLevel splits on sep outside brackets, braces, parentheses and angles.
func splitTopLevel(text string, sep rune) []string {
	var parts []string
	depth, start := 0, 0
	for index, character := range text {
		switch character {
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, text[start:index])
				start = index + 1
			}
		}
	}
	return append(parts, text[start:])
}

// normalizeName compares identifiers: prefix and quote dropped, trimmed.
func normalizeName(value string) string {
	text := strings.TrimSpace(value)
	if index := strings.LastIndex(text, ":"); index >= 0 && !strings.Contains(text, "/") {
		text = text[index+1:]
	}
	text = strings.TrimSuffix(strings.TrimSpace(text), "()")
	return strings.TrimPrefix(text, "'")
}

// normalizePath canonicalises a resource path: leading '/', quotes and
// backslash escapes (chat\.postMessage) dropped, every path parameter
// spelled `:name`.
func normalizePath(value string) string {
	text := strings.Trim(strings.ReplaceAll(strings.TrimSpace(value), `\`, ""), "/")
	segments := strings.Split(text, "/")
	for index, segment := range segments {
		segment = strings.TrimSpace(segment)
		if match := pathParam.FindStringSubmatch(segment); match != nil {
			for _, group := range match[1:] {
				if group != "" {
					segment = ":" + group
				}
			}
		}
		segments[index] = strings.TrimPrefix(segment, "'")
	}
	return strings.Join(segments, "/")
}

// normalizeValue compares default values: quotes and prefixes dropped, numbers
// compared numerically.
func normalizeValue(value string) string {
	text := strings.Trim(strings.TrimSpace(value), `"'`)
	text = normalizeName(text)
	if number, err := strconv.ParseFloat(text, 64); err == nil {
		return strconv.FormatFloat(number, 'g', -1, 64)
	}
	return text
}

// normalizeLibrary compares org/name coordinates without a version.
func normalizeLibrary(value string) string {
	text := strings.TrimSpace(value)
	if index := strings.Index(text, ":"); index >= 0 {
		text = text[:index]
	}
	return strings.ToLower(text)
}
