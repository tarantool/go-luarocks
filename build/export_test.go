package build

import (
	"context"

	rocks "github.com/tarantool/go-luarocks"
)

// This file bridges unexported identifiers (and the shared test helpers in
// helpers_test.go) to the external `build_test` package so the white-box
// tests can exercise them through an exported surface while themselves
// living in `_test` packages (testpackage).

// Test helpers re-exported for external (build_test) tests.

// WriteFile exposes writeFile for external tests.
func WriteFile(path, content string) error { return writeFile(path, content) }

// ReadFile exposes readFile for external tests.
func ReadFile(path string) ([]byte, error) { return readFile(path) }

// ChmodX exposes chmodX for external tests.
func ChmodX(path string) error { return chmodX(path) }

// FileExists exposes fileExists for external tests.
func FileExists(path string) bool { return fileExists(path) }

// Contains exposes contains for external tests.
func Contains(haystack, needle string) bool { return contains(haystack, needle) }

// Unexported package functions re-exported for external tests.

// DeriveFlagsFor exposes deriveFlagsFor for external tests.
func DeriveFlagsFor(cfg rocks.Config, goos string) Flags { return deriveFlagsFor(cfg, goos) }

// MergeEnv exposes mergeEnv for external tests.
func MergeEnv(base []string, overlay map[string]string) []string { return mergeEnv(base, overlay) }

// SplitEnv exposes splitEnv for external tests.
func SplitEnv(e string) (string, string, bool) { return splitEnv(e) }

// BuildEnv exposes buildEnv for external tests.
func BuildEnv(cfg rocks.Config) map[string]string { return buildEnv(cfg) }

// RunCmd exposes runCmd for external tests.
func RunCmd(ctx context.Context, name string, args []string, cwd string, extraEnv map[string]string) error {
	return runCmd(ctx, name, args, cwd, extraEnv)
}

// SubstituteVars exposes substituteVars for external tests.
func SubstituteVars(s string, vars map[string]string) string { return substituteVars(s, vars) }

// BuildVars exposes buildVars for external tests.
func BuildVars(spec *rocks.Rockspec, cfg rocks.Config) map[string]string {
	return buildVars(spec, cfg)
}
