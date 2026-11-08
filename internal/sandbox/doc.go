// Package sandbox holds the canary test of the Docker test sandbox started by
// scripts/test-in-docker.sh. The test checks that the sandbox really denies
// writes outside the temp dirs and, when started offline, network access, so
// a weakened sandbox fails loudly instead of silently letting stray writes
// through. Outside the sandbox the test skips.
package sandbox
