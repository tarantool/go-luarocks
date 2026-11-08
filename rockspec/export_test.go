package rockspec

// Test-only exports of unexported helpers so external (rockspec_test)
// unit tests can exercise them directly.

// ParseDepStringForTest exposes parseDepString to the test package.
var ParseDepStringForTest = parseDepString

// DepStringMatchesSchemaForTest exposes depStringMatchesSchema to the test
// package.
var DepStringMatchesSchemaForTest = depStringMatchesSchema

// RuntimePlatformsForTest exposes runtimePlatformsFor to the test package.
var RuntimePlatformsForTest = runtimePlatformsFor

// ErrPackageNotPathComponent exposes errPackageNotPathComponent to the test
// package.
var ErrPackageNotPathComponent = errPackageNotPathComponent
