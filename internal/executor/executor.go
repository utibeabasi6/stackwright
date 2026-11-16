// Package executor runs one job inside a sandbox: it claims the job, checks
// and runs the engine, reports each output line as an event, and uploads the
// artifacts.
package executor

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/utibeabasi6/stackwright/internal/artifact"
	"github.com/utibeabasi6/stackwright/internal/egress"
	"github.com/utibeabasi6/stackwright/internal/graph"
	"github.com/utibeabasi6/stackwright/internal/redact"
	"github.com/utibeabasi6/stackwright/internal/sts"
)

var (
	ErrEngineChecksum = errors.New("the engine binary does not match its checksum")
	ErrInputChecksum  = errors.New("the input does not match the checksum the run recorded")
	errNotFound       = errors.New("not found")
)

// credentialLifetime is how long AWS credentials last. A job that runs
// longer cannot renew them.
const (
	credentialLifetime = time.Hour
	scanTimeout        = 5 * time.Minute
)

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
		secrets: redact.New(job.GetToken(), job.GetStateToken(), job.GetAws().GetWebIdentityToken())}
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

	binPath, cached, err := FetchEngine(ctx, r.job.GetEngineBinary(), r.cache, r.work)
	if err != nil {
		return &stageError{"engine", err}
	}
	r.emit("executor.engine_ready", map[string]any{"sha256": job.GetEngineBinary().GetSha256(), "cached": cached})

	root := filepath.Join(r.work, "src")
	inputs := map[string][]byte{}
	for _, in := range job.GetInputs() {
		sealed, err := download(ctx, in.GetUrl())
		if errors.Is(err, errNotFound) && (in.GetName() == "lock.hcl" || in.GetName() == "workdir.tar.gz") {
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
	// An apply starts from the tree its plan left, so files that the plan
	// wrote, such as an archive_file zip, are there. Plans from before the
	// tree was kept have only the source.
	tree, ok := inputs["workdir.tar.gz"]
	if !ok {
		tree = inputs["source.tar.gz"]
	}
	if err := untar(tree, root); err != nil {
		return &stageError{"source", err}
	}
	dir, err := within(root, job.GetProjectPath())
	if err != nil {
		return &stageError{"source", err}
	}
	// The addresses go in the environment, not here: a plan file keeps its
	// backend block, and the apply may run where this host does not resolve.
	// A project on the code's own backend gets no override (adr/0036).
	plan, lock := filepath.Join(dir, "plan.bin"), filepath.Join(dir, ".terraform.lock.hcl")
	files := map[string][]byte{}
	if !job.GetCodeBackend() {
		files[filepath.Join(dir, "stackwright_override.tf")] = []byte("terraform {\n  backend \"http\" {\n    username = \"stackwright\"\n  }\n}\n")
	}
	if b, ok := inputs["plan.bin"]; ok && !job.GetTerragrunt() {
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
	if job.GetStack() != nil {
		stackEnv, err := r.stack(root, dir)
		if err != nil {
			return &stageError{"stack", err}
		}
		env = append(env, stackEnv...)
	}
	address := r.serverURL + job.GetStatePath()
	// Stackwright's own entries come last: with a key twice, the last one
	// wins, so a variable cannot replace them.
	env = append(env, "HOME="+home, "PATH=/usr/local/bin:/usr/bin:/bin", "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	env = append(env, egress.Env()...)
	if job.GetCodeBackend() {
		if job.GetWorkspace() != "" {
			env = append(env, "TF_WORKSPACE="+job.GetWorkspace())
		}
	} else {
		env = append(env, "TF_HTTP_PASSWORD="+cmp.Or(job.GetStateToken(), job.GetToken()), "TF_HTTP_ADDRESS="+address, "TF_HTTP_LOCK_ADDRESS="+address, "TF_HTTP_UNLOCK_ADDRESS="+address)
	}
	if providers := filepath.Join(r.cache, "providers"); r.cache != "" && isDir(providers) {
		config := filepath.Join(home, "cli.tfrc")
		if err := os.WriteFile(config, fmt.Appendf(nil, "provider_installation {\n  filesystem_mirror {\n    path = %q\n  }\n  direct {}\n}\n", providers), 0o600); err != nil {
			return &stageError{"source", err}
		}
		env = append(env, "TF_CLI_CONFIG_FILE="+config)
	}
	if job.GetTerragrunt() {
		if !job.GetCodeBackend() {
			return &stageError{"source", errors.New("a Terragrunt stack runs on the backends its remote_state names, not on Stackwright's")}
		}
		return r.terragrunt(ctx, binPath, dir, root, env, inputs["plan.bin"])
	}
	initJSON := job.GetEngine() == v1.Engine_ENGINE_OPENTOFU
	if !initJSON {
		// Terraform's init writes JSON only in newer versions; its help says which.
		help, _ := exec.CommandContext(ctx, binPath, "init", "-help").CombinedOutput() //nolint:gosec // the engine binary, checked against its SHA-256
		initJSON = bytes.Contains(help, []byte("-json"))
	}
	var planFlags []string
	if job.GetRefreshOnly() {
		planFlags = append(planFlags, "-refresh-only")
	}
	if job.GetDestroy() {
		planFlags = append(planFlags, "-destroy")
	}
	// A removed Stack instance's module gets null inputs, which a refresh
	// would evaluate (adr/0043).
	if job.GetStack().GetRemoved() {
		planFlags = append(planFlags, "-refresh=false")
	}
	engine := &cli{
		bin: binPath, dir: dir, initJSON: initJSON, redact: r.redact, env: env, planFlags: planFlags,
		line: func(e event) {
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
		outputs, err := engine.outputs(ctx)
		if err != nil {
			return &stageError{"outputs", err}
		}
		if err := r.upload(ctx, "outputs.json", outputs); err != nil {
			return &stageError{"artifacts", err}
		}
		return nil
	}
	if r.hasChanges, err = engine.plan(ctx, plan); err != nil {
		return &stageError{"plan", err}
	}
	// Code the plan ran, such as an external data source, can leave a process
	// behind that swaps plan.bin after show reads it.
	killStrays()
	planJSON, err := engine.show(ctx, plan)
	if err != nil {
		return &stageError{"plan", err}
	}
	outputs := map[string]string{"plan.bin": plan, "lock.hcl": lock}
	if job.GetPlanOnly() {
		delete(outputs, "plan.bin")
	}
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
	return r.finishPlan(ctx, planJSON, root, dir, plan)
}

// finishPlan uploads a plan's JSON, its security scan, the source map and
// the tree the plan left, without skip.
func (r *runner) finishPlan(ctx context.Context, planJSON []byte, root, dir, skip string) error {
	job := r.job
	if err := r.upload(ctx, "plan.json", planJSON); err != nil {
		return &stageError{"artifacts", err}
	}
	// A scan that fails fails the plan: a plan with no review must not
	// pass as one with no findings (adr/0037).
	scan, err := r.scan(ctx, planJSON, job)
	if err != nil {
		return &stageError{"scan", err}
	}
	if err := r.upload(ctx, "scan.json", scan); err != nil {
		return &stageError{"artifacts", err}
	}
	// A file that does not parse only costs the graph its links, so the plan
	// goes on without it.
	sources, err := graph.ReadSources(root, dir)
	if err != nil {
		r.emit("executor.sources_failed", map[string]any{"error": string(r.redact([]byte(err.Error())))})
	}
	b, _ := json.Marshal(sources)
	if err := r.upload(ctx, "sources.json", b); err != nil {
		return &stageError{"artifacts", err}
	}
	if job.GetPlanOnly() {
		return nil
	}
	workdir, err := tarTree(root, skip)
	if err != nil {
		return &stageError{"artifacts", err}
	}
	if err := r.upload(ctx, "workdir.tar.gz", workdir); err != nil {
		return &stageError{"artifacts", err}
	}
	return nil
}

// scan runs Trivy on the plan JSON with its own checks and the org's, with
// no network, and returns its JSON report (adr/0037).
func (r *runner) scan(ctx context.Context, planJSON []byte, job *v1.Job) ([]byte, error) {
	dir := filepath.Join(r.work, "scan")
	checks := filepath.Join(dir, "checks")
	if err := os.MkdirAll(checks, 0o700); err != nil {
		return nil, err
	}
	plan, ignore := filepath.Join(dir, "plan.json"), filepath.Join(dir, "skipped")
	if err := os.WriteFile(plan, planJSON, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(ignore, []byte(strings.Join(job.GetSkippedChecks(), "\n")), 0o600); err != nil {
		return nil, err
	}
	args := []string{"config", "--skip-check-update", "--quiet", "--format", "json", "--cache-dir", filepath.Join(dir, "cache"), "--ignorefile", ignore}
	for _, p := range job.GetPolicies() {
		if err := os.WriteFile(filepath.Join(checks, filepath.Base(p.GetName())+".rego"), []byte(p.GetRego()), 0o600); err != nil {
			return nil, err
		}
	}
	if len(job.GetPolicies()) > 0 {
		args = append(args, "--config-check", checks, "--check-namespaces", "user")
	}
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "trivy", append(args, plan)...) //nolint:gosec // the scanner in the image, with arguments this function builds
	cmd.Env = []string{"HOME=" + filepath.Join(r.work, "home"), "PATH=/usr/local/bin:/usr/bin:/bin"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("the security scan failed: %w: %s", err, r.redact(stderr.Bytes()))
	}
	return out, nil
}

// engineEnv is the workspace's variables, then the cloud credentials. The
// engine gets nothing else from the executor's environment.
func (r *runner) engineEnv(ctx context.Context) ([]string, error) {
	var env []string
	for _, v := range r.job.GetVariables() {
		if r.job.GetStack() != nil && v.GetKey() == stackComponents {
			continue
		}
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

// FetchEngine returns the engine's path: the builder's cached copy when its
// SHA-256 matches, or else a download into work.
func FetchEngine(ctx context.Context, d *v1.Download, cache, work string) (path string, cached bool, err error) {
	if cache != "" {
		path := filepath.Join(cache, "engines", d.GetSha256(), filepath.Base(d.GetName()))
		if b, err := os.ReadFile(path); err == nil { //nolint:gosec // a path under the read-only cache
			if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) == d.GetSha256() {
				return path, true, nil
			}
		}
	}
	bin, err := download(ctx, d.GetUrl())
	if err != nil {
		return "", false, err
	}
	if sum := sha256.Sum256(bin); hex.EncodeToString(sum[:]) != d.GetSha256() {
		return "", false, ErrEngineChecksum
	}
	path = filepath.Join(work, "bin", filepath.Base(d.GetName()))
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
	if len(sealed) > artifact.MaxSize {
		return fmt.Errorf("%s: %w", name, artifact.ErrTooLarge)
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

func download(ctx context.Context, url string) ([]byte, error) {
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
		case tar.TypeSymlink:
			// A link stands for configuration, which must not go missing
			// from the plan; one that leaves the source is refused.
			target := path.Clean(path.Join(path.Dir(filepath.ToSlash(name)), h.Linkname)) //nolint:gosec // checked on the next line, and os.Root keeps the link in dir
			if path.IsAbs(h.Linkname) || target == ".." || strings.HasPrefix(target, "../") {
				return fmt.Errorf("the link %s points outside the source, to %s", h.Name, h.Linkname)
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				return err
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		}
	}
}

// tarTree packs dir as untar reads it: folders and regular files. It leaves
// out every .terraform folder, which init makes again, and the file skip.
func tarTree(dir, skip string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir || path == skip {
			return err
		}
		if d.IsDir() && (d.Name() == ".terraform" || d.Name() == ".terragrunt-cache") {
			return filepath.SkipDir
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(h); err != nil || d.IsDir() {
			return err
		}
		f, err := os.Open(path) //nolint:gosec // a path inside dir
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func within(root, rel string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the source folder", rel)
	}
	return path, nil
}

// killStrays kills every other process in the sandbox. It acts only as the
// container's init, where every other process belongs to the job.
func killStrays() {
	if os.Getpid() != 1 {
		return
	}
	for range 10 {
		entries, _ := os.ReadDir("/proc")
		killed := false
		for _, e := range entries {
			if pid, err := strconv.Atoi(e.Name()); err == nil && pid != 1 {
				killed = syscall.Kill(pid, syscall.SIGKILL) == nil || killed
			}
		}
		if !killed {
			return
		}
		time.Sleep(50 * time.Millisecond)
		for {
			if pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil); pid <= 0 || err != nil {
				break
			}
		}
	}
}
