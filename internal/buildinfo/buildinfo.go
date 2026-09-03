// Package buildinfo exposes build-identity metadata derived from the
// binary's embedded VCS build info (runtime/debug.ReadBuildInfo).
package buildinfo

import "runtime/debug"

const (
	appName  = "gogen"
	fallback = "dev"
)

// userAgent is computed once: build identity is fixed for the process
// lifetime, so there is no point re-reading the build info per request.
var userAgent = computeUserAgent()

// UserAgent returns the User-Agent string GoGen sends to external services
// (OpenAI-compatible APIs, web fetch). It is "gogen/<7-char git revision>"
// when the binary was built from a git checkout (go build embeds VCS info
// since Go 1.18) and "gogen/dev" otherwise (zip/tarball builds or
// -buildvcs=false).
func UserAgent() string {
	return userAgent
}

func computeUserAgent() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return appName + "/" + fallback
	}
	for _, st := range bi.Settings {
		if st.Key == "vcs.revision" && len(st.Value) >= 7 {
			return appName + "/" + st.Value[:7]
		}
	}
	return appName + "/" + fallback
}
