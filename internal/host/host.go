// Package host classifies cask download URLs as slow-path (throttled GitHub
// release assets that brewfast can accelerate) or already CDN-fast.
package host

import (
	"net/url"
	"strings"
)

// releaseAssetsHost is the redirect target GitHub serves release-asset
// downloads from. Any https URL on this host is treated as slow-path.
const releaseAssetsHost = "release-assets.githubusercontent.com"

// IsSlowHost reports whether rawurl is a throttled GitHub release asset that
// brewfast can accelerate. It is a pure function and performs no I/O.
//
// It returns true only when the scheme is https AND the URL is either:
//   - a GitHub release asset: host github.com with a path matching
//     /<owner>/<repo>/releases/download/...
//   - the redirect target host release-assets.githubusercontent.com (any path)
//
// A non-https scheme is never slow-path. Any other host, a non-release
// github.com path, or a malformed URL yields false without panicking.
func IsSlowHost(rawurl string) bool {
	u, err := url.Parse(rawurl)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}

	switch u.Hostname() {
	case "github.com":
		return isReleaseDownloadPath(u.Path)
	case releaseAssetsHost:
		return true
	default:
		return false
	}
}

// isReleaseDownloadPath reports whether p is a GitHub release-asset download
// path: /<owner>/<repo>/releases/download/<...>.
func isReleaseDownloadPath(p string) bool {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	// owner, repo, "releases", "download", plus at least one more segment.
	if len(segs) < 5 {
		return false
	}
	if segs[0] == "" || segs[1] == "" {
		return false
	}
	return segs[2] == "releases" && segs[3] == "download"
}
