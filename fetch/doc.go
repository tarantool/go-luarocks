// Package fetch retrieves a rock's `source.url` into a working directory.
//
// Fetch is the default-options entry point; FetchWith takes Options carrying
// the rockspec's source metadata (tag/branch for git, md5/file for archives)
// plus transport tweaks (insecure hosts, User-Agent). Both return the on-disk
// path of the unpacked working tree.
//
// The dispatcher selects a Backend per URL scheme:
//
//	http, https                          → http.go (net/http GET + unpack)
//	git, git+http, git+https, git+ssh,
//	git+file                             → git.go  (go-git clone, no binary)
//	file                                 → file.go (copy local tree)
//
// Unknown schemes return ErrUnsupportedRockspecFeature wrapped with the
// scheme name.
//
// All backends honor ctx for cancellation at the network/transport level
// — the HTTP request and the go-git clone are ctx-bound. Note that
// local archive extraction after an HTTP fetch is not interrupted mid-unpack.
// None mutate process state — no os.Setenv, no os.Chdir.
package fetch
