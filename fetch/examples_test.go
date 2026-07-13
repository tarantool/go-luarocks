package fetch_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/tarantool/go-luarocks/fetch"
)

// ExampleFetch retrieves a rock source over the file:// scheme by copying a
// local directory tree into destDir.
func ExampleFetch() {
	srcDir, err := os.MkdirTemp("", "src")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(srcDir)

	dstDir, err := os.MkdirTemp("", "dst")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dstDir)

	if err := os.WriteFile(filepath.Join(srcDir, "hello.lua"), []byte("return 42\n"), 0o644); err != nil {
		log.Fatal(err)
	}

	got, err := fetch.Fetch(context.Background(), "file://"+srcDir, dstDir)
	if err != nil {
		log.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(got, "hello.lua"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(string(body))
	// Output: return 42
}

// ExampleFetchWith is the options-bearing form: source.tag / source.md5 /
// insecure hosts / user-agent all ride on Options. (Uses the network for an
// http(s) URL; shown for documentation.)
func ExampleFetchWith() {
	_, err := fetch.FetchWith(
		context.Background(),
		"https://example.com/rock-1.0.tar.gz",
		"/tmp/dest",
		fetch.Options{MD5: "d41d8cd98f00b204e9800998ecf8427e", UserAgent: "go-luarocks"},
	)
	if err != nil {
		log.Fatal(err)
	}
}
