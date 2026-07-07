package rockspec

// Test-only exports of unexported helpers so external (rockspec_test)
// unit tests can exercise them directly.

// ParseDepStringForTest exposes parseDepString to the test package.
var ParseDepStringForTest = parseDepString

// RuntimePlatformsForTest exposes runtimePlatformsFor to the test package.
var RuntimePlatformsForTest = runtimePlatformsFor
