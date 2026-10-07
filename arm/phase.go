package arm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Phase is a named subset of the benchmark matrix.
type Phase struct {
	Description string   `json:"description"`
	Arms        []string `json:"arms"`
	Scenarios   []string `json:"scenarios"`
	Runs        int      `json:"runs"`
	Model       string   `json:"model"`
}

// LoadPhases reads arms/phases.json.
func LoadPhases(dir string) (map[string]Phase, error) {
	content, err := os.ReadFile(filepath.Join(dir, "phases.json"))
	if err != nil {
		return nil, err
	}
	phases := map[string]Phase{}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&phases); err != nil {
		return nil, fmt.Errorf("decode phases.json: %w", err)
	}
	return phases, nil
}

// PhaseNames lists phase names sorted.
func PhaseNames(phases map[string]Phase) []string {
	names := make([]string, 0, len(phases))
	for name := range phases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
