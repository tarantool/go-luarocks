package hostarch

// This file bridges unexported identifiers to the external `hostarch_test`
// package so white-box tests can exercise them while the test files
// themselves stay in `_test` packages (testpackage).

// SystemName exposes systemName for external tests, so every GOOS branch can
// be exercised regardless of the host running the test suite.
func SystemName(goos string) string { return systemName(goos) }

// ProcessorName exposes processorName for external tests, so every GOARCH
// branch can be exercised regardless of the host running the test suite.
func ProcessorName(goarch string) string { return processorName(goarch) }
