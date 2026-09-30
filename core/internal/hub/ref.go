package hub

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// isCommitHash is a length and hex check, not a parse: the Hub is the only
// authority on whether a revision exists, and a wrong guess costs one failed
// request.
func isCommitHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// escapePath escapes each segment but keeps the separators, because a resolve
// URL addresses a file inside a repository, not a single flat name.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func totalOf(files []File) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// RepoRef is a convenience for callers that only have "owner/name".
func RepoRef(ref, revision string) (owner, name, rev string, err error) {
	parts := strings.Split(strings.Trim(ref, "/"), "/")
	switch len(parts) {
	case 2:
		owner, name, rev = parts[0], parts[1], revision
	case 3:
		owner, name, rev = parts[0], parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("hub: %q is not owner/name or owner/name@revision", ref)
	}
	if rev == "" {
		rev = "main"
	}
	if owner == "" || name == "" {
		return "", "", "", fmt.Errorf("hub: %q has an empty owner or name", ref)
	}
	if path.Base(owner) != owner || path.Base(name) != name {
		return "", "", "", fmt.Errorf("hub: %q must be two path segments", ref)
	}
	return owner, name, rev, nil
}
