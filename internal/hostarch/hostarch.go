// Package hostarch names the running host as LuaRocks does. LuaRocks calls
// the platform a binary rock is built for its arch, cfg.arch, which is
// "<system>-<processor>" ("linux-x86_64", "macosx-aarch64") and ends the
// name of every binary rock. The names come from the Go runtime, the platform
// this program was built for, so both backends name the host alike: the
// native one filters manifests and labels the rocks it packs with Arch, and
// the lua one hands System and Processor to the embedded LuaRocks as
// hardcoded.SYSTEM and hardcoded.PROCESSOR, which cfg.init takes over what
// luarocks.core.sysdetect reads off the first architecture of /bin/sh.
package hostarch

import "runtime"

// Arch returns the LuaRocks cfg.arch string of the running host,
// "<System>-<Processor>", e.g. "linux-x86_64".
func Arch() string {
	return System() + "-" + Processor()
}

// System returns the LuaRocks name of the running host's operating system,
// e.g. "linux" or "macosx".
func System() string {
	return systemName(runtime.GOOS)
}

// Processor returns the LuaRocks name of the running host's processor, e.g.
// "x86_64" or "aarch64".
func Processor() string {
	return processorName(runtime.GOARCH)
}

// systemName maps a GOOS onto the name luarocks.core.sysdetect gives the
// system, best-effort: every GOOS but darwin passes through unchanged.
func systemName(goos string) string {
	switch goos {
	case "darwin":
		return "macosx"
	default:
		return goos
	}
}

// processorName maps a GOARCH onto the name luarocks.core.sysdetect gives the
// processor, best-effort: every GOARCH but amd64, 386 and arm64 passes through
// unchanged.
func processorName(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "386":
		return "x86"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}
