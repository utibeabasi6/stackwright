// Package sandbox starts isolated environments for jobs. Docker is the only
// backend today; others implement Backend (adr/0007).
package sandbox

import (
	"context"
	"time"
)

// FilesDir is where a sandbox finds Spec.Files.
const FilesDir = "/run/stackwright"

type Spec struct {
	Image   string
	Command []string
	// Env must not hold secrets: a backend may show it to the host.
	Env map[string]string
	// Files are written to FilesDir before the sandbox starts. Secrets go
	// here.
	Files  map[string][]byte
	Labels map[string]string
}

type Backend interface {
	// Platform names the sandboxes' OS and architecture, such as linux_arm64.
	Platform(ctx context.Context) (string, error)
	Start(ctx context.Context, spec Spec) (Sandbox, error)
}

type Sandbox interface {
	ID() string
	// Wait returns the exit code.
	Wait(ctx context.Context) (int32, error)
	Logs(ctx context.Context) ([]byte, error)
	// Stop asks the sandbox to stop and kills it after grace.
	Stop(ctx context.Context, grace time.Duration) error
	// Remove stops the sandbox at once and deletes everything it made.
	Remove(ctx context.Context) error
}
