package prober

import (
	"net/url"
	"strings"
)

// joinURLPath returns the Path and RawPath of base's path without its trailing "/", followed by
// ref's path. rabbitMgmtURL and promQueryURL both build on it.
//
// Only LITERAL trailing slashes are trimmed, and the decoded Path loses exactly as many "/" as the
// escaped path does (second external review of iter-0210, P2-2). A base ending in an escaped slash,
// such as "/vhosts/%2F", decodes to "/vhosts/". Trimming "/" from both forms independently left
// Path "/vhosts" against RawPath "/vhosts/%2F": unescape(RawPath) != Path, so net/url discarded
// RawPath and re-encoded Path, and every %2F in the request was lost. The invariant kept here is
// unescape(RawPath) == Path, which makes net/url send RawPath as built.
func joinURLPath(base, ref *url.URL) (path, rawPath string) {
	escaped := base.EscapedPath()
	trimmed := strings.TrimRight(escaped, "/")
	n := len(escaped) - len(trimmed)
	// The n literal slashes that end the escaped form decode to the last n bytes of Path.
	basePath := base.Path
	if n <= len(basePath) && strings.HasSuffix(basePath, strings.Repeat("/", n)) {
		basePath = basePath[:len(basePath)-n]
	}
	return basePath + ref.Path, trimmed + ref.EscapedPath()
}
