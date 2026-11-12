// Package guard holds the path rule that the toolbox and the agent share.
// It imports nothing of Stackwright's, so the agent can use it without a
// path to the executor (adr/0031).
package guard

import (
	"path"
	"strings"
)

// Refused reports why no tool may touch rel, a slash path from the
// repository root, or "" when a tool may.
func Refused(rel string) string {
	for _, part := range strings.Split(path.Clean("/" + rel)[1:], "/") {
		switch {
		case part == ".git" || part == ".terraform":
			return part + " is not for tools"
		case strings.HasSuffix(part, ".tfstate") || strings.HasSuffix(part, ".tfstate.backup"):
			return "state files hold secrets and are never read or written"
		case part == ".env" || strings.HasPrefix(part, ".env."):
			return ".env files hold secrets and are never read or written"
		}
	}
	return ""
}
