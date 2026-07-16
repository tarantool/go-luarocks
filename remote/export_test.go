package remote

// This file bridges unexported identifiers to the external `remote_test`
// package so white-box tests can exercise them while the test files
// themselves stay in `_test` packages (testpackage).

// LuaOSName exposes luaOSName for external tests, so every GOOS branch can
// be exercised regardless of the host running the test suite.
func LuaOSName(goos string) string { return luaOSName(goos) }

// LuaCPUName exposes luaCPUName for external tests, so every GOARCH branch
// can be exercised regardless of the host running the test suite.
func LuaCPUName(goarch string) string { return luaCPUName(goarch) }
