package remote_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tarantool/go-luarocks/remote"
)

// exampleRelease is where rocks-repo publishes the rocks of hello 1.0-1.
const exampleRelease = "https://github.com/org/rocks-repo/releases/download/hello-1.0-1/"

// ExampleRocksIndex_Add lists a version in a new index — its rockspec beside
// the index, its rock on a release page — and writes the index out. Listing
// the version again is refused: a published version never changes.
func ExampleRocksIndex_Add() {
	idx := remote.NewRocksIndex()

	hello := remote.IndexVersion{
		Rockspec: &remote.IndexFile{URL: "hello-1.0-1.rockspec", SHA256: strings.Repeat("a", 64)},
		Artifacts: []remote.IndexArtifact{{
			Arch: "all", URL: exampleRelease + "hello-1.0-1.all.rock", SHA256: strings.Repeat("B", 64), Size: 1234,
		}},
	}

	if err := idx.Add("hello", "1.0-1", hello); err != nil {
		panic(err)
	}

	err := idx.Add("hello", "1.0-1", hello)
	fmt.Println(errors.Is(err, remote.ErrDuplicateVersion))

	if err := idx.Write(os.Stdout); err != nil {
		panic(err)
	}
	// Output:
	// true
	// {
	//   "format": 1,
	//   "repository": {
	//     "hello": {
	//       "1.0-1": {
	//         "artifacts": [
	//           {
	//             "arch": "all",
	//             "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	//             "size": 1234,
	//             "url": "https://github.com/org/rocks-repo/releases/download/hello-1.0-1/hello-1.0-1.all.rock"
	//           }
	//         ],
	//         "rockspec": {
	//           "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	//           "url": "hello-1.0-1.rockspec"
	//         }
	//       }
	//     }
	//   }
	// }
}

// ExampleRocksIndex_Validate reads an index and reports every entry that
// breaks its rules, each naming the entry: here a rock whose url names
// another file, and a rockspec published over plain http.
func ExampleRocksIndex_Validate() {
	idx, err := remote.ReadRocksIndex(strings.NewReader(`{
  "format": 1,
  "repository": {
    "hello": {"1.0-1": {"artifacts": [
      {"arch": "all", "url": "hello-1.0-1.rock", "sha256": "` + strings.Repeat("a", 64) + `"}
    ]}},
    "world": {"1.0-1": {"rockspec":
      {"url": "http://rocks.example/world-1.0-1.rockspec", "sha256": "` + strings.Repeat("b", 64) + `"}
    }}
  }
}`))
	if err != nil {
		panic(err)
	}

	fmt.Println(idx.Validate(context.Background(), remote.ValidateOptions{}))
	// Output:
	// hello 1.0-1: artifact all: url "hello-1.0-1.rock" names the file "hello-1.0-1.rock", want "hello-1.0-1.all.rock"
	// world 1.0-1: rockspec: url "http://rocks.example/world-1.0-1.rockspec" is plain http, allowed only for a host among the insecure servers
}
