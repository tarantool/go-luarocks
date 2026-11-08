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
// Everything a fetch writes stays inside destDir, a deliberate strictness
// beyond upstream: a source.file, a file name from the URL, or a clone
// directory name that leaves it — absolute, "..", or "." where a file is
// meant — fails the fetch with an error wrapping rocks.ErrUnsafePath (a
// clone named ".." is cloned as "repo" instead), and so does an archive
// entry reaching above destDir or beneath a symlink pointing out of it. An
// entry with an absolute name is extracted inside destDir, as tar and unzip
// do. Extraction creates no symlinks and keeps only the permission bits of
// an entry, never setuid, setgid or sticky.
//
// All backends honor ctx for cancellation at the network/transport level
// — the HTTP request and the go-git clone are ctx-bound. Note that
// local archive extraction after an HTTP fetch is not interrupted mid-unpack.
// None mutate process state — no os.Setenv, no os.Chdir.
package fetch
