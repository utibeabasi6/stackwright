// Package executor runs one job inside a sandbox: it claims the job, checks
// and runs the engine, reports each output line as an event, and uploads the
// artifacts.
package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/artifact"
	"github.com/stackwright/stackwright/internal/redact"
	"github.com/stackwright/stackwright/internal/sts"
)

var (
	ErrEngineChecksum = errors.New("the engine binary does not match its checksum")
	ErrInputChecksum  = errors.New("the input does not match the checksum the run recorded")
	errNotFound       = errors.New("not found")
)

// credentialLifetime is how long AWS credentials last. A job that runs
// longer cannot renew them.
const credentialLifetime = time.Hour

// Run claims the job that ticket names and runs it in workDir. cacheDir is
// the builder's read-only cache, or empty.
func Run(ctx context.Context, serverURL, ticket, workDir, cacheDir string) error {
	serverURL = strings.TrimRight(serverURL, "/")
	client := stackwrightv1connect.NewExecutorServiceClient(http.DefaultClient, serverURL)
	claim := connect.NewRequest(&v1.ClaimJobRequest{})
	claim.Header().Set("Authorization", "Bearer "+ticket)
	resp, err := client.ClaimJob(ctx, claim)
	if err != nil {
		return fmt.Errorf("claim job: %w", err)
	}
	job := resp.Msg.GetJob()
	slog.Info("job claimed", "job", job.GetId(), "operation", job.GetOperation().String())

	ctx, cancel := context.WithDeadline(ctx, job.GetDeadline().AsTime())
	defer cancel()
	rep := &reporter{client: client, token: job.GetToken(), lost: cancel}
	reportCtx, stopReporting := context.WithCancel(context.WithoutCancel(ctx))
	defer stopReporting()
	go rep.run(reportCtx)

	r := &runner{job: job, rep: rep, serverURL: serverURL, work: workDir, cache: cacheDir,
		secrets: redact.New(job.GetToken(), job.GetAws().GetWebIdentityToken())}
	for _, v := range job.GetVariables() {
		if v.GetSensitive() {
			r.secrets.Add(v.GetValue())
		}
	}
	r.emit("executor.started", map[string]any{
		"operation": job.GetOperation().String(), "engine": job.GetEngine().String(), "version": job.GetEngineVersion(),
	})
	runErr := r.run(ctx)
	if runErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		runErr = fmt.Errorf("the job ran past its time limit: %w", runErr)
	}
	if err := r.upload(context.WithoutCancel(ctx), "log.jsonl", r.log.Bytes()); err != nil {
		slog.Error("upload log", "err", err)
	}
	msg := ""
	if runErr != nil {
		msg = string(r.redact([]byte(runErr.Error())))
	}
	r.emit("executor.finished", map[string]any{"succeeded": runErr == nil, "has_changes": r.hasChanges, "error": msg})

	stopReporting()
	end, cancelEnd := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancelEnd()
	if err := rep.flush(end); err != nil {
		return errors.Join(runErr, fmt.Errorf("report events: %w", err))
	}
	finish := connect.NewRequest(&v1.FinishJobRequest{Succeeded: runErr == nil, Error: msg, HasChanges: r.hasChanges})
	finish.Header().Set("Authorization", "Bearer "+job.GetToken())
	if _, err := client.FinishJob(end, finish); err != nil {
		return errors.Join(&redactedError{msg: msg, err: runErr}, fmt.Errorf("finish job: %w", err))
	}
	if runErr != nil {
		return &redactedError{msg: msg, err: runErr}
	}
	return nil
}

// redactedError keeps the cause for errors.Is and prints only the masked
// text, so the sandbox's log holds no secret.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

type runner struct {
	job        *v1.Job
	rep        *reporter
	serverURL  string
	work       string
	cache      string
	secrets    *redact.Redactor
	log        bytes.Buffer
	hasChanges bool
}

type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func (r *runner) redact(b []byte) []byte {
	return r.secrets.Bytes(b)
}

func (r *runner) emit(typ string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	r.rep.emit(typ, "", b)
}

func (r *runner) run(ctx context.Context) (err error) {
	defer func() {
		if se, ok := errors.AsType[*stageError](err); ok {
			r.emit("executor.failed", map[string]any{"stage": se.stage, "error": string(r.redact([]byte(se.err.Error())))})
		}
	}()
	job := r.job

	binPath, cached, err := r.engine(ctx)
	if err != nil {
		return &stageError{"engine", err}
	}
	r.emit("executor.engine_ready", map[string]any{"sha256": job.GetEngineBinary().GetSha256(), "cached": cached})

	root := filepath.Join(r.work, "src")
	inputs := map[string][]byte{}
	for _, in := range job.GetInputs() {
		sealed, err := r.download(ctx, in.GetUrl())
		if errors.Is(err, errNotFound) && in.GetName() == "lock.hcl" {
			continue
		}
		if err != nil {
			return &stageError{"inputs", fmt.Errorf("%s: %w", in.GetName(), err)}
		}
		plain, err := artifact.Open(job.GetArtifactKey(), in.GetJobId(), in.GetName(), sealed)
		if err != nil {
			return &stageError{"inputs", fmt.Errorf("%s: %w", in.GetName(), err)}
		}
		if sum := sha256.Sum256(plain); in.GetSha256() != "" && hex.EncodeToString(sum[:]) != in.GetSha256() {
			return &stageError{"inputs", fmt.Errorf("%s: %w", in.GetName(), ErrInputChecksum)}
		}
		inputs[in.GetName()] = plain
	}
	if err := untar(inputs["source.tar.gz"], root); err != nil {
		return &stageError{"source", err}
	}
	dir, err := within(root, job.GetProjectPath())
	if err != nil {
		return &stageError{"source", err}
	}
	address := r.serverURL + job.GetStatePath()
	override := fmt.Sprintf("terraform {\n  backend \"http\" {\n    address        = %q\n    lock_address   = %q\n    unlock_address = %q\n    username       = \"stackwright\"\n  }\n}\n",
		address, address, address)
	plan, lock := filepath.Join(dir, "plan.bin"), filepath.Join(dir, ".terraform.lock.hcl")
	files := map[string][]byte{filepath.Join(dir, "stackwright_override.tf"): []byte(override)}
	if b, ok := inputs["plan.bin"]; ok {
		files[plan] = b
	}
	if b, ok := inputs["lock.hcl"]; ok {
		files[lock] = b
	}
	for path, b := range files {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return &stageError{"source", err}
		}
	}

	home := filepath.Join(r.work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return &stageError{"source", err}
	}
	env, err := r.engineEnv(ctx)
	if err != nil {
		return &stageError{"credentials", err}
	}
	// Stackwright's own entries come last: with a key twice, the last one
	// wins, so a variable cannot replace them.
	env = append(env, "HOME="+home, "PATH=/usr/local/bin:/usr/bin:/bin", "TF_IN_AUTOMATION=1", "TF_INPUT=0", "TF_HTTP_PASSWORD="+job.GetToken())
	if providers := filepath.Join(r.cache, "providers"); r.cache != "" && isDir(providers) {
		config := filepath.Join(home, "cli.tfrc")
		if err := os.WriteFile(config, fmt.Appendf(nil, "provider_installation {\n  filesystem_mirror {\n    path = %q\n  }\n  direct {}\n}\n", providers), 0o600); err != nil {
			return &stageError{"source", err}
		}
		env = append(env, "TF_CLI_CONFIG_FILE="+config)
	}
	initJSON := job.GetEngine() == v1.Engine_ENGINE_OPENTOFU
	if !initJSON {
		// Terraform's init writes JSON only in newer versions; its help says which.
		help, _ := exec.CommandContext(ctx, binPath, "init", "-help").CombinedOutput() //nolint:gosec // the engine binary, checked against its SHA-256
		initJSON = bytes.Contains(help, []byte("-json"))
	}
	engine := &cli{
		bin: binPath, dir: dir, initJSON: initJSON, redact: r.redact, env: env,
		line: func(e event) {
			r.log.Write(e.payload)
			r.log.WriteByte('\n')
			r.rep.emit(e.typ, e.resourceAddr, e.payload)
		},
	}
	if err := engine.init(ctx); err != nil {
		return &stageError{"init", err}
	}

	if job.GetOperation() == v1.JobOperation_JOB_OPERATION_APPLY {
		if err := engine.apply(ctx, plan); err != nil {
			return &stageError{"apply", err}
		}
		return nil
	}
	if r.hasChanges, err = engine.plan(ctx, plan); err != nil {
		return &stageError{"plan", err}
	}
	planJSON, err := engine.show(ctx, plan)
	if err != nil {
		return &stageError{"plan", err}
	}
	outputs := map[string]string{"plan.bin": plan, "lock.hcl": lock}
	for name, path := range outputs {
		b, err := os.ReadFile(path) //nolint:gosec // a path this function built
		if name == "lock.hcl" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return &stageError{"artifacts", err}
		}
		if err := r.upload(ctx, name, b); err != nil {
			return &stageError{"artifacts", err}
		}
	}
	if err := r.upload(ctx, "plan.json", planJSON); err != nil {
		return &stageError{"artifacts", err}
	}
	return nil
}

// engineEnv is the workspace's variables, then the cloud credentials. The
// engine gets nothing else from the executor's environment.
func (r *runner) engineEnv(ctx context.Context) ([]string, error) {
	var env []string
	for _, v := range r.job.GetVariables() {
		switch v.GetCategory() {
		case v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM:
			env = append(env, "TF_VAR_"+v.GetKey()+"="+v.GetValue())
		case v1.VariableCategory_VARIABLE_CATEGORY_ENV:
			env = append(env, v.GetKey()+"="+v.GetValue())
		}
	}
	aws := r.job.GetAws()
	if aws == nil {
		return env, nil
	}
	creds, err := sts.Exchange(ctx, aws.GetStsEndpoint(), aws.GetRoleArn(), "stackwright-"+r.job.GetId(), aws.GetWebIdentityToken(), credentialLifetime)
	if err != nil {
		return nil, err
	}
	r.secrets.Add(creds.SecretAccessKey, creds.SessionToken)
	r.emit("executor.credentials_issued", map[string]any{
		"provider": "aws", "role_arn": aws.GetRoleArn(), "access_key_id": creds.AccessKeyID, "expires_at": creds.Expiration,
	})
	return append(env,
		"AWS_ACCESS_KEY_ID="+creds.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+creds.SecretAccessKey,
		"AWS_SESSION_TOKEN="+creds.SessionToken,
		"AWS_REGION="+aws.GetRegion(),
		"AWS_DEFAULT_REGION="+aws.GetRegion(),
	), nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// engine returns the engine's path: the builder's cached copy when its
// SHA-256 matches, or else a download.
func (r *runner) engine(ctx context.Context) (path string, cached bool, err error) {
	d := r.job.GetEngineBinary()
	if r.cache != "" {
		path := filepath.Join(r.cache, "engines", d.GetSha256(), filepath.Base(d.GetName()))
		if b, err := os.ReadFile(path); err == nil { //nolint:gosec // a path under the read-only cache
			if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) == d.GetSha256() {
				return path, true, nil
			}
		}
	}
	bin, err := r.download(ctx, d.GetUrl())
	if err != nil {
		return "", false, err
	}
	if sum := sha256.Sum256(bin); hex.EncodeToString(sum[:]) != d.GetSha256() {
		return "", false, ErrEngineChecksum
	}
	path = filepath.Join(r.work, "bin", filepath.Base(d.GetName()))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, bin, 0o700); err != nil { //nolint:gosec // the engine must be executable
		return "", false, err
	}
	return path, false, nil
}

func (r *runner) upload(ctx context.Context, name string, data []byte) error {
	url, ok := r.job.GetUploads()[name]
	if !ok {
		return fmt.Errorf("the job has no upload URL for %s", name)
	}
	sealed, err := artifact.Seal(r.job.GetArtifactKey(), r.job.GetId(), name, data)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(sealed))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload %s: %s", name, resp.Status)
	}
	return nil
}

func (r *runner) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s", resp.Status)
	}
	const limit = 512 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && len(data) > limit {
		err = fmt.Errorf("download is larger than %d bytes", limit)
	}
	return data, err
}

// untar unpacks a .tar.gz into dir. os.Root refuses entries that would land
// outside it. Links and special files are skipped.
func untar(archive []byte, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimPrefix(h.Name, "/"))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				return err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, 256<<20))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

func within(root, rel string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the source folder", rel)
	}
	return path, nil
}
