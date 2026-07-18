package api

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var obsoleteAllocationRoutePattern = regexp.MustCompile(`/(?:v1/)?allocate\b`)

func TestPublicAllocationDocsUseRegisteredRoutes(t *testing.T) {
	root := documentationRepositoryRoot(t)
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	for _, route := range []string{
		"POST /v1/allocations",
		"GET /v1/allocations/{id}",
		"POST /v1/allocations/{id}/cancel",
		"POST /v1/allocations/{id}/complete",
	} {
		if !strings.Contains(string(readme), route) {
			t.Errorf("README.md does not document registered route %q", route)
		}
	}

	for _, path := range publicDocumentationFiles(t, root) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if match := findObsoleteAllocationRoute(content); match != nil {
			relativePath, relativeErr := filepath.Rel(root, path)
			if relativeErr != nil {
				relativePath = path
			}
			t.Errorf("%s contains obsolete allocation route %q", relativePath, match)
		}
	}
}

func TestObsoleteAllocationRoutePattern(t *testing.T) {
	for input, wantMatch := range map[string]bool{
		"POST /allocate":                  true,
		"POST http://broker/v1/allocate":  true,
		"POST /v1/allocate?pool=full":     true,
		"POST /v1/allocations":            false,
		"GET /v1/allocations/alloc-123":   false,
		"uses: ./actions/allocate-runner": false,
	} {
		if got := findObsoleteAllocationRoute([]byte(input)) != nil; got != wantMatch {
			t.Errorf("obsolete route match for %q = %t, want %t", input, got, wantMatch)
		}
	}
}

func findObsoleteAllocationRoute(content []byte) []byte {
	for _, location := range obsoleteAllocationRoutePattern.FindAllIndex(content, -1) {
		if bytes.HasPrefix(content[location[1]:], []byte("-runner")) {
			continue
		}
		return content[location[0]:location[1]]
	}
	return nil
}

func documentationRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve docs contract test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func publicDocumentationFiles(t *testing.T, root string) []string {
	t.Helper()
	paths := []string{filepath.Join(root, "README.md")}
	for _, directory := range []string{"docs", "examples"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".json", ".md", ".sh", ".yaml", ".yml":
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan public documentation in %s: %v", directory, err)
		}
	}
	return paths
}
