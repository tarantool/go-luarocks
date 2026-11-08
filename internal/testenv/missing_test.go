package testenv_test

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tarantool/go-luarocks/internal/testenv"
)

// recorder is a testing.TB that records how a test was ended. Like the real
// thing, Fatalf and Skipf stop the calling goroutine, so it records exactly
// the calls that would end a real test.
type recorder struct {
	testing.TB

	calls []string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.calls = append(r.calls, "fatal: "+fmt.Sprintf(format, args...))

	runtime.Goexit()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.calls = append(r.calls, "skip: "+fmt.Sprintf(format, args...))

	runtime.Goexit()
}

// ended runs end against a recorder on a goroutine of its own, as the testing
// package runs a test, and returns what it recorded.
func ended(end func(tb testing.TB)) []string {
	r := &recorder{}
	done := make(chan struct{})

	go func() {
		defer close(done)

		end(r)
	}()

	<-done

	return r.calls
}

func TestMissingf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []string
	}{
		{
			name:  "empty skips",
			value: "",
			want:  []string{"skip: tarantool not found on PATH: boom"},
		},
		{
			name:  "zero skips",
			value: "0",
			want:  []string{"skip: tarantool not found on PATH: boom"},
		},
		{
			name:  "one fails",
			value: "1",
			want: []string{"fatal: missing prerequisite (" + testenv.StrictEnv +
				"=1): tarantool not found on PATH: boom"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(testenv.StrictEnv, tc.value)

			assert.Equal(t, tc.value == "1", testenv.Strict(), "Strict")
			assert.Equal(t, tc.want, ended(func(tb testing.TB) {
				tb.Helper()
				testenv.Missingf(tb, "tarantool not found on PATH: %v", "boom")
			}), "how the test ended")
		})
	}
}

func TestNoNetworkf(t *testing.T) {
	const msg = "luarocks.org:443 not reachable"

	for _, tc := range []struct {
		name    string
		strict  string
		offline string
		want    []string
	}{
		{
			name: "online skips",
			want: []string{"skip: " + msg},
		},
		{
			name:   "online strict fails",
			strict: "1",
			want:   []string{"fatal: missing prerequisite (" + testenv.StrictEnv + "=1): " + msg},
		},
		{
			name:    "offline skips",
			offline: "1",
			want:    []string{"skip: offline run (" + testenv.OfflineEnv + "=1): " + msg},
		},
		{
			name:    "offline strict skips",
			strict:  "1",
			offline: "1",
			want:    []string{"skip: offline run (" + testenv.OfflineEnv + "=1): " + msg},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(testenv.StrictEnv, tc.strict)
			t.Setenv(testenv.OfflineEnv, tc.offline)

			assert.Equal(t, tc.offline == "1", testenv.Offline(), "Offline")
			assert.Equal(t, tc.want, ended(func(tb testing.TB) {
				tb.Helper()
				testenv.NoNetworkf(tb, "%s", msg)
			}), "how the test ended")
		})
	}
}
