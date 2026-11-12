package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// A provider line is "source version", such as
// "registry.opentofu.org/hashicorp/random 3.6.0".
var providerLine = regexp.MustCompile(`^([a-z0-9.-]+)/([a-z0-9_-]+)/([a-z0-9_-]+) ([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*)$`)

// Mirror downloads providers into cacheDir/providers with the engine's
// `providers mirror`, which checks each provider's signature. The builder
// runs it in a sandbox that may write the cache; job sandboxes cannot.
func Mirror(ctx context.Context, engine, engineSHA string, providers []string, cacheDir, workDir string) error {
	b, err := os.ReadFile(engine) //nolint:gosec // a path under the cache, checked against its SHA-256 below
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != engineSHA {
		return ErrEngineChecksum
	}
	var config strings.Builder
	config.WriteString("terraform {\n  required_providers {\n")
	for i, p := range providers {
		m := providerLine.FindStringSubmatch(p)
		if m == nil {
			return fmt.Errorf("provider %q is not \"source version\"", p)
		}
		fmt.Fprintf(&config, "    p%d = {\n      source  = %q\n      version = %q\n    }\n", i, m[1]+"/"+m[2]+"/"+m[3], "= "+m[4])
	}
	config.WriteString("  }\n}\n")
	if err := os.WriteFile(filepath.Join(workDir, "main.tf"), []byte(config.String()), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, engine, "providers", "mirror", "-platform="+runtime.GOOS+"_"+runtime.GOARCH, filepath.Join(cacheDir, "providers")) //nolint:gosec // the engine binary, checked against its SHA-256
	cmd.Dir = workDir
	cmd.Env = []string{"HOME=" + workDir, "TF_IN_AUTOMATION=1"}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("providers mirror: %w: %s", err, out)
	}
	return nil
}
