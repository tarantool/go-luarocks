package fetch_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/tarantool/go-luarocks/fetch"
)

// ExampleFetch retrieves a rock source over the file:// scheme by copying a
// local directory tree into destDir.
func ExampleFetch() {
	srcDir, err := os.MkdirTemp("", "src")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(srcDir) }()

	dstDir, err := os.MkdirTemp("", "dst")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dstDir) }()

	if err := os.WriteFile(filepath.Join(srcDir, "hello.lua"), []byte("return 42\n"), 0o644); err != nil {
		panic(err)
	}

	got, err := fetch.Fetch(context.Background(), "file://"+srcDir, dstDir)
	if err != nil {
		panic(err)
	}

	body, err := os.ReadFile(filepath.Join(got, "hello.lua"))
	if err != nil {
		panic(err)
	}

	fmt.Print(string(body))
	// Output: return 42
}

// ExampleFetchWith is the options-bearing form: source.tag / source.md5 /
// insecure hosts / user-agent all ride on Options. Here the http backend
// downloads from an in-process server and verifies the payload's md5 before
// accepting it.
func ExampleFetchWith() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("return { answer = 42 }\n"))
	}))
	defer srv.Close()

	dstDir, err := os.MkdirTemp("", "dst")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dstDir) }()

	got, err := fetch.FetchWith(
		context.Background(),
		srv.URL+"/answer.lua",
		dstDir,
		fetch.Options{MD5: "9a411841b564fa0fc78745f8de8f6340", UserAgent: "go-luarocks"},
	)
	if err != nil {
		panic(err)
	}

	body, err := os.ReadFile(filepath.Join(got, "answer.lua"))
	if err != nil {
		panic(err)
	}

	fmt.Print(string(body))
	// Output: return { answer = 42 }
}
