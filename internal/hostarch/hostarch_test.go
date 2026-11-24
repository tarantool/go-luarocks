package hostarch_test

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tarantool/go-luarocks/internal/hostarch"
)

// TestSystemName covers every branch of the unexported systemName switch.
func TestSystemName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "macosx", hostarch.SystemName("darwin"))
	assert.Equal(t, "linux", hostarch.SystemName("linux"))
	assert.Equal(t, "windows", hostarch.SystemName("windows"), "unmapped GOOS values pass through unchanged")
}

// TestProcessorName covers every branch of the unexported processorName
// switch.
func TestProcessorName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "x86_64", hostarch.ProcessorName("amd64"))
	assert.Equal(t, "x86", hostarch.ProcessorName("386"))
	assert.Equal(t, "aarch64", hostarch.ProcessorName("arm64"))
	assert.Equal(t, "riscv64", hostarch.ProcessorName("riscv64"), "unmapped GOARCH values pass through unchanged")
}

// TestArch: the arch is the system and the processor of the running host,
// joined by a dash.
func TestArch(t *testing.T) {
	t.Parallel()

	assert.Equal(t, hostarch.SystemName(runtime.GOOS), hostarch.System())
	assert.Equal(t, hostarch.ProcessorName(runtime.GOARCH), hostarch.Processor())
	assert.Equal(t, hostarch.System()+"-"+hostarch.Processor(), hostarch.Arch())
}
