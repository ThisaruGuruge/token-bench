package arm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// latestDir holds discover's cached "latest version" answers, each
// {"version": ..., "atMs": ...}; an entry older than the tool's TTL (10 min)
// is re-resolved against Central.
const latestDir = "v2/latest"

func isLatestEntry(relative string) bool {
	slashed := filepath.ToSlash(relative)
	return strings.HasPrefix(slashed, latestDir+"/") && strings.HasSuffix(slashed, ".json")
}

// CopySeed copies a discover cache seed into destination and re-stamps every
// latest-version entry with now, so unpinned lookups stay on the seeded
// versions for the tool's whole TTL. It returns the files copied.
func CopySeed(seed, destination string, now time.Time) (int, error) {
	files := 0
	err := filepath.WalkDir(seed, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(seed, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("cache seed holds a non-regular file: %s", relative)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isLatestEntry(relative) {
			if content, err = restamp(content, now.UnixMilli()); err != nil {
				return fmt.Errorf("re-stamp %s: %w", relative, err)
			}
		}
		files++
		return os.WriteFile(target, content, 0o644)
	})
	return files, err
}

// restamp sets atMs, keeping every other key.
func restamp(content []byte, atMs int64) ([]byte, error) {
	var entry map[string]any
	if err := json.Unmarshal(content, &entry); err != nil {
		return nil, err
	}
	if _, ok := entry["version"].(string); !ok {
		return nil, fmt.Errorf("no version")
	}
	entry["atMs"] = atMs
	return json.Marshal(entry)
}

// HashSeed hashes a seed's relative paths and contents, with latest-entry
// timestamps zeroed, and counts its files.
func HashSeed(seed string) (string, int, error) {
	hash := sha256.New()
	files := 0
	err := filepath.WalkDir(seed, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, err := filepath.Rel(seed, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isLatestEntry(relative) {
			if content, err = restamp(content, 0); err != nil {
				return fmt.Errorf("%s: %w", relative, err)
			}
		}
		files++
		fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(relative), len(content))
		hash.Write(content)
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil))[:16], files, err
}

// NewCacheFiles counts files under cache that the seed does not have, or
// whose content differs from the seed's (latest entries compared without
// their timestamp): the run's live fetches from Central.
func NewCacheFiles(seed, cache string) (int, error) {
	count := 0
	err := filepath.WalkDir(cache, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, err := filepath.Rel(cache, path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(filepath.Join(seed, relative))
		if err != nil {
			count++
			return nil
		}
		if isLatestEntry(relative) {
			got, _ = restamp(got, 0)
			want, _ = restamp(want, 0)
		}
		if !bytes.Equal(got, want) {
			count++
		}
		return nil
	})
	return count, err
}
