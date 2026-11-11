package remote

// The writing side of a tt-rocks-index.json, for the CI that publishes one:
// list a version, check the whole index, and write it out the same way
// every time, so the commits that change it show only what changed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/transport"
)

// ErrDuplicateVersion is what Add fails with for a version the index already
// lists.
var ErrDuplicateVersion = errors.New("remote: version already listed")

// NewRocksIndex returns an empty index of the format this package writes.
func NewRocksIndex() *RocksIndex {
	return &RocksIndex{Format: RocksIndexFormat, Repository: map[string]map[string]IndexVersion{}}
}

// Add lists v as version of the rock name. It refuses, with an error
// wrapping ErrDuplicateVersion, a version the index already lists for the
// rock, spelled the same or comparing equal (deps.Compare): a published
// version is never changed, and a rebuild is a new revision. It refuses an
// entry that breaks the rules of RocksIndex as a reader with no insecure
// servers applies them, so a plain http url too. The entry is stored as a
// copy, its digests lowercased and its artifacts sorted by arch.
func (idx *RocksIndex) Add(name, version string, v IndexVersion) error {
	if _, err := checkIndexVersion(name, version, v, nil); err != nil {
		return fmt.Errorf("remote: add %s %s: %w", name, version, err)
	}

	parsed, _ := deps.ParseVersion(version) // checkIndexVersion parsed it already

	for listed := range idx.Repository[name] {
		other, err := deps.ParseVersion(listed)
		if listed == version || (err == nil && deps.Compare(parsed, other) == 0) {
			return fmt.Errorf("remote: add %s %s: %w as %s", name, version, ErrDuplicateVersion, listed)
		}
	}

	if idx.Repository == nil {
		idx.Repository = map[string]map[string]IndexVersion{}
	}

	if idx.Repository[name] == nil {
		idx.Repository[name] = map[string]IndexVersion{}
	}

	idx.Repository[name][version] = normalizedVersion(v)

	return nil
}

// normalizedVersion is a copy of v sharing nothing with it, its digests
// lowercased and its artifacts sorted by arch.
func normalizedVersion(v IndexVersion) IndexVersion {
	if v.Rockspec != nil {
		spec := *v.Rockspec
		spec.SHA256 = strings.ToLower(spec.SHA256)
		v.Rockspec = &spec
	}

	v.Artifacts = sortedArtifacts(v.Artifacts)
	for i := range v.Artifacts {
		v.Artifacts[i].SHA256 = strings.ToLower(v.Artifacts[i].SHA256)
	}

	return v
}

// sortedArtifacts is a copy of artifacts sorted by arch.
func sortedArtifacts(artifacts []IndexArtifact) []IndexArtifact {
	out := slices.Clone(artifacts)
	slices.SortFunc(out, func(a, b IndexArtifact) int { return strings.Compare(a.Arch, b.Arch) })

	return out
}

// Write writes the index as JSON that depends on its contents alone: every
// object's keys sorted, each version's artifacts sorted by arch, two-space
// indentation, no HTML escaping (an "&" in a url stays one) and a final
// newline — the form `jq -S .` gives it, so the same index is the same bytes
// and a diff of two shows only what changed. It refuses an index whose
// format is not one this package writes.
func (idx *RocksIndex) Write(w io.Writer) error {
	if idx.Format < 1 || idx.Format > RocksIndexFormat {
		return fmt.Errorf("remote: write rock index: format %d is not one this package writes (1 to %d)",
			idx.Format, RocksIndexFormat)
	}

	out := RocksIndex{Format: idx.Format, Repository: make(map[string]map[string]IndexVersion, len(idx.Repository))}

	for name, versions := range idx.Repository {
		sorted := make(map[string]IndexVersion, len(versions))

		for version, v := range versions {
			v.Artifacts = sortedArtifacts(v.Artifacts)
			sorted[version] = v
		}

		out.Repository[name] = sorted
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("remote: write rock index: %w", err)
	}

	return nil
}

// ValidateOptions tunes RocksIndex.Validate.
type ValidateOptions struct {
	// InsecureServers lists the hosts an absolute http url may name, as
	// IndexOptions.InsecureServers does for a reader, and, with Fetch, the
	// hosts whose TLS certificates are not verified.
	InsecureServers []string

	// Fetch downloads every file the index lists and compares its sha256,
	// and its size when the entry gives one, with the entry. Without it,
	// Validate reads nothing but the index.
	Fetch bool

	// Base is where the index is served from, for Fetch to resolve relative
	// urls against: the URL of its directory, or a local directory, as a
	// rock server is configured. It is needed only when the index lists a
	// relative url.
	Base string

	// UserAgent overrides the User-Agent header of Fetch's requests.
	UserAgent string
}

// Validate checks the index against the rules of RocksIndex — its format,
// and every entry, as a reader with opts.InsecureServers reads them — and,
// with opts.Fetch, that every file it lists is there with the digest and
// size it lists. It reports every problem it finds, each naming its entry,
// joined into one error (errors.Join), rather than stopping at the first:
// it is what a linter of the index runs. A valid index is a nil error.
func (idx *RocksIndex) Validate(ctx context.Context, opts ValidateOptions) error {
	var problems []error

	if idx.Format < 1 || idx.Format > RocksIndexFormat {
		problems = append(problems, fmt.Errorf("format %d is not one this package reads (1 to %d)",
			idx.Format, RocksIndexFormat))
	}

	if idx.Repository == nil {
		problems = append(problems, errors.New("the index has no repository"))
	}

	base, err := validationBase(opts.Base, opts.InsecureServers)
	if err != nil && opts.Fetch {
		return err
	}

	for _, name := range slices.Sorted(maps.Keys(idx.Repository)) {
		versions := idx.Repository[name]

		for _, version := range slices.Sorted(maps.Keys(versions)) {
			files, err := checkIndexVersion(name, version, versions[version], opts.InsecureServers)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s %s: %w", name, version, err))

				continue
			}

			if !opts.Fetch {
				continue
			}

			for _, f := range files {
				if err := checkPublished(ctx, base, f, opts); err != nil {
					problems = append(problems, fmt.Errorf("%s %s: %s: %w", name, version, f.arch, err))
				}
			}
		}
	}

	return errors.Join(problems...)
}

// validationBase is the indexBase relative urls resolve against when
// Validate fetches: server, a directory URL or a local directory. An empty
// server resolves nothing: a relative url then fails to fetch.
func validationBase(server string, insecure []string) (indexBase, error) {
	if server == "" {
		return indexBase{insecure: insecure}, nil
	}

	if dir, ok := LocalServerPath(server); ok {
		return indexBase{dir: dir, prefix: filePrefixOf(server), insecure: insecure}, nil
	}

	web, err := url.Parse(strings.TrimRight(server, "/") + "/")
	if err != nil {
		return indexBase{}, fmt.Errorf("remote: validate: base %q: %w", server, err)
	}

	return indexBase{web: web, insecure: insecure}, nil
}

// checkPublished fetches the file f names and compares its digest, and its
// size when f gives one, with f.
func checkPublished(ctx context.Context, base indexBase, f indexedFile, opts ValidateOptions) error {
	if f.parsed.Scheme == "" && base.web == nil && base.dir == "" {
		return fmt.Errorf("relative url %q, and no base to resolve it against", f.ref)
	}

	at := base.locate(f)

	body, err := openPublished(ctx, at, opts)
	if err != nil {
		return err
	}

	defer func() {
		transport.Drain(body)
		_ = body.Close()
	}()

	h := sha256.New()

	size, err := io.Copy(h, body)
	if err != nil {
		return fmt.Errorf("read %s: %w", at, err)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != f.sha256 {
		return fmt.Errorf("%s has sha256 %s, the index lists %s", at, got, f.sha256)
	}

	if f.size != 0 && size != f.size {
		return fmt.Errorf("%s has %d bytes, the index lists %d", at, size, f.size)
	}

	return nil
}

// openPublished opens the file at at, a URL or a local path as
// indexBase.locate returns it.
func openPublished(ctx context.Context, at string, opts ValidateOptions) (io.ReadCloser, error) {
	if p, ok := LocalServerPath(at); ok {
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", p, err)
		}

		return f, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, at, nil)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", at, err)
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}

	req.Header.Set("User-Agent", ua)

	resp, err := transport.NewClient(opts.InsecureServers, httpClientTimeout, 0).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", at, err)
	}

	if resp.StatusCode >= httpStatusErrorThreshold {
		transport.Drain(resp.Body)
		_ = resp.Body.Close()

		return nil, &statusError{url: at, code: resp.StatusCode}
	}

	return resp.Body, nil
}
