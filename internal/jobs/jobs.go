package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/internal/artifact"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/engine"
	"github.com/stackwright/stackwright/internal/oidc"
	"github.com/stackwright/stackwright/internal/seal"
	"github.com/stackwright/stackwright/internal/variables"
)

const (
	// HeartbeatEvery is how often an executor reports, with or without
	// events. A job that misses three is lost.
	HeartbeatEvery = 15 * time.Second
	lostAfter      = 3 * HeartbeatEvery
	unclaimedFor   = 10 * time.Minute
	timeLimit      = 2 * time.Hour
)

var (
	ErrNoPlan     = errors.New("an apply needs a succeeded plan with changes, of the same workspace")
	ErrNoArtifact = errors.New("the job did not upload this artifact")
)

type Service struct {
	Pool *pgxpool.Pool
	KEK  seal.KEK
	S3   *minio.Client
	// Presign signs URLs for the built-in builder and its sandboxes, and
	// PresignRemote for other build servers. Each endpoint is the one they
	// reach, which can differ from S3's.
	Presign       *minio.Client
	PresignRemote *minio.Client
	Bucket        string
	Engines       *engine.Mirror
	// Issuer signs the web identity token of a job whose workspace has a
	// cloud account. STSEndpoint is where the executor trades it.
	Issuer      *oidc.Issuer
	STSEndpoint string
	// OnEnd runs in the transaction that ends a job, with the org setting.
	// status is succeeded, failed or lost.
	OnEnd func(ctx context.Context, tx pgx.Tx, jobID, status, msg string) error
}

type Request struct {
	OrgID       string
	WorkspaceID string
	CreatedBy   string
	Operation   v1.JobOperation
	// Platform is the sandbox's, such as linux_arm64.
	Platform string
	// Source is a .tar.gz of the repository, for a plan.
	Source []byte
	// PlanJobID is the plan that an apply applies. The apply uses its source,
	// engine and key.
	PlanJobID string
	RunID     string
	// BuildServerID runs the job. Remote is false for the built-in one.
	BuildServerID string
	Remote        bool
}

// Created is a queued job and what its builder needs to start it.
type Created struct {
	ID     string
	Ticket string
	// Engine is presigned for the builder, which caches it.
	Engine *v1.Download
}

func (s *Service) presigner(remote bool) *minio.Client {
	if remote {
		return s.PresignRemote
	}
	return s.Presign
}

func objectKey(orgID, jobID, name string) string {
	return fmt.Sprintf("orgs/%s/jobs/%s/%s", orgID, jobID, name)
}

func (s *Service) inOrg(ctx context.Context, orgID string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, orgID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Create queues a job and returns the one-time ticket that its sandbox
// claims it with.
func (s *Service) Create(ctx context.Context, r Request) (Created, error) {
	var eng, version, platform, sourceSHA string
	var sealedKey []byte
	err := s.inOrg(ctx, r.OrgID, func(tx pgx.Tx) error {
		var cloud bool
		if err := tx.QueryRow(ctx, `SELECT cloud_account_id IS NOT NULL FROM workspaces WHERE id = $1`, r.WorkspaceID).Scan(&cloud); err != nil {
			return err
		}
		if cloud && !strings.HasPrefix(s.Issuer.URL, "https://") {
			return fmt.Errorf("the workspace has a cloud account, but %w (it is %s)", oidc.ErrIssuer, s.Issuer.URL)
		}
		switch r.Operation {
		case v1.JobOperation_JOB_OPERATION_PLAN:
			platform = r.Platform
			return tx.QueryRow(ctx, `SELECT p.engine, p.engine_version FROM workspaces w JOIN projects p ON p.id = w.project_id
				WHERE w.id = $1 AND w.archived_at IS NULL AND p.archived_at IS NULL`, r.WorkspaceID).Scan(&eng, &version)
		case v1.JobOperation_JOB_OPERATION_APPLY:
			err := tx.QueryRow(ctx, `SELECT engine, engine_version, platform, artifact_key, source_sha256 FROM jobs
				WHERE id = $1 AND workspace_id = $2 AND operation = 'plan' AND status = 'succeeded' AND has_changes`,
				r.PlanJobID, r.WorkspaceID).Scan(&eng, &version, &platform, &sealedKey, &sourceSHA)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNoPlan
			}
			return err
		}
		return fmt.Errorf("unknown job operation %s", r.Operation)
	})
	if err != nil {
		return Created{}, err
	}
	engine, err := s.EngineDownload(ctx, eng, version, platform, r.Remote)
	if err != nil {
		return Created{}, err
	}

	jobID, ticket := uuid.NewString(), auth.RandomToken()
	err = s.inOrg(ctx, r.OrgID, func(tx pgx.Tx) error {
		operation, planJobID := "apply", &r.PlanJobID
		if r.Operation == v1.JobOperation_JOB_OPERATION_PLAN {
			operation, planJobID = "plan", nil
			sum := sha256.Sum256(r.Source)
			sourceSHA = hex.EncodeToString(sum[:])
			key := artifact.NewKey()
			var err error
			if sealedKey, err = s.KEK.Seal(ctx, tx, r.OrgID, key); err != nil {
				return err
			}
			blob, err := artifact.Seal(key, jobID, "source.tar.gz", r.Source)
			if err != nil {
				return err
			}
			if _, err := s.S3.PutObject(ctx, s.Bucket, objectKey(r.OrgID, jobID, "source.tar.gz"), bytes.NewReader(blob), int64(len(blob)),
				minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
				return fmt.Errorf("store job source: %w", err)
			}
		}
		deadline := time.Now().Add(timeLimit)
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, org_id, workspace_id, operation, plan_job_id, engine, engine_version, platform,
			artifact_key, created_by, deadline, run_id, source_sha256, build_server_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, '')::uuid, $11, nullif($12, '')::uuid, $13, nullif($14, '')::uuid)`,
			jobID, r.OrgID, r.WorkspaceID, operation, planJobID, eng, version, platform, sealedKey, r.CreatedBy, deadline,
			r.RunID, sourceSHA, r.BuildServerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO job_leases (job_id, org_id, workspace_id, ticket_hash, expires_at) VALUES ($1, $2, $3, $4, $5)`,
			jobID, r.OrgID, r.WorkspaceID, auth.HashToken(ticket), deadline)
		return err
	})
	if err != nil {
		return Created{}, err
	}
	return Created{ID: jobID, Ticket: ticket, Engine: engine}, nil
}

// EngineDownload makes sure the engine is in the mirror and returns its
// download, presigned for a builder on the built-in host or a remote one.
func (s *Service) EngineDownload(ctx context.Context, eng, version, platform string, remote bool) (*v1.Download, error) {
	bin, err := s.Engines.Ensure(ctx, eng, version, platform)
	if err != nil {
		return nil, fmt.Errorf("engine %s %s: %w", eng, version, err)
	}
	u, err := s.presigner(remote).PresignedGetObject(ctx, s.Bucket, bin.ObjectKey, time.Hour, url.Values{})
	if err != nil {
		return nil, err
	}
	return &v1.Download{Name: bin.Name, Url: u.String(), Sha256: bin.SHA256}, nil
}

// Claim starts a job whose ticket the caller spent, and returns everything
// its sandbox needs. tx needs the org setting.
func (s *Service) Claim(ctx context.Context, tx pgx.Tx, jobID string) (*v1.Job, error) {
	token := auth.NewJobToken()
	if _, err := tx.Exec(ctx, `UPDATE job_leases SET token_hash = $2, heartbeat_at = now() WHERE job_id = $1`, jobID, auth.HashToken(token)); err != nil {
		return nil, err
	}
	var (
		orgID, operation, eng, version, platform string
		orgSlug, project, projectPath, workspace string
		sourceSHA, projectID, workspaceID        string
		runID                                    *string
		remote                                   bool
		planJobID                                *string
		sealedKey                                []byte
		deadline                                 time.Time
	)
	err := tx.QueryRow(ctx, `UPDATE jobs j SET status = 'running', claimed_at = now()
		FROM workspaces w, projects p, orgs o
		WHERE j.id = $1 AND j.status = 'queued' AND w.id = j.workspace_id AND p.id = w.project_id AND o.id = j.org_id
		RETURNING j.org_id, j.operation, j.plan_job_id, j.engine, j.engine_version, j.platform, j.artifact_key, j.deadline,
			j.source_sha256, o.slug, p.slug, p.path, w.name, p.id, w.id, j.run_id,
			EXISTS (SELECT FROM build_servers b WHERE b.id = j.build_server_id AND b.org_id IS NOT NULL)`, jobID).
		Scan(&orgID, &operation, &planJobID, &eng, &version, &platform, &sealedKey, &deadline,
			&sourceSHA, &orgSlug, &project, &projectPath, &workspace, &projectID, &workspaceID, &runID, &remote)
	if err != nil {
		return nil, fmt.Errorf("claim job %s: %w", jobID, err)
	}
	key, err := s.KEK.Open(ctx, tx, orgID, sealedKey)
	if err != nil {
		return nil, err
	}
	var binKey, binSHA string
	if err := tx.QueryRow(ctx, `SELECT object_key, sha256 FROM engines WHERE engine = $1 AND version = $2 AND platform = $3`,
		eng, version, platform).Scan(&binKey, &binSHA); err != nil {
		return nil, err
	}

	expires := time.Until(deadline)
	presign := s.presigner(remote)
	get := func(objKey string) (string, error) {
		u, err := presign.PresignedGetObject(ctx, s.Bucket, objKey, expires, url.Values{})
		if err != nil {
			return "", err
		}
		return u.String(), nil
	}
	binURL, err := get(binKey)
	if err != nil {
		return nil, err
	}
	job := &v1.Job{
		Id:            jobID,
		Token:         token,
		Operation:     v1.JobOperation_JOB_OPERATION_PLAN,
		Engine:        v1.Engine_ENGINE_OPENTOFU,
		EngineVersion: version,
		EngineBinary:  &v1.Download{Name: path.Base(binKey), Url: binURL, Sha256: binSHA},
		StatePath:     fmt.Sprintf("/state/%s/%s/%s", orgSlug, project, workspace),
		ProjectPath:   projectPath,
		ArtifactKey:   key,
		Uploads:       map[string]string{},
		Deadline:      timestamppb.New(deadline),
	}
	if eng == "terraform" {
		job.Engine = v1.Engine_ENGINE_TERRAFORM
	}
	sourceJob, inputs, outputs := jobID, []string{"source.tar.gz"}, []string{"plan.bin", "plan.json", "lock.hcl", "sources.json", "workdir.tar.gz", "log.jsonl"}
	if operation == "apply" {
		job.Operation = v1.JobOperation_JOB_OPERATION_APPLY
		sourceJob, inputs, outputs = *planJobID, []string{"source.tar.gz", "workdir.tar.gz", "plan.bin", "lock.hcl"}, []string{"log.jsonl"}
	}
	for _, name := range inputs {
		u, err := get(objectKey(orgID, sourceJob, name))
		if err != nil {
			return nil, err
		}
		d := &v1.Download{Name: name, Url: u, JobId: sourceJob}
		if name == "source.tar.gz" {
			d.Sha256 = sourceSHA
		}
		job.Inputs = append(job.Inputs, d)
	}
	for _, name := range outputs {
		u, err := presign.PresignedPutObject(ctx, s.Bucket, objectKey(orgID, jobID, name), expires)
		if err != nil {
			return nil, err
		}
		job.Uploads[name] = u.String()
	}

	vars, err := variables.Effective(ctx, tx, workspaceID)
	if err != nil {
		return nil, err
	}
	for _, v := range vars {
		jv := &v1.JobVariable{Key: v.Key, Sensitive: v.Sensitive, Category: v1.VariableCategory_VARIABLE_CATEGORY_TERRAFORM}
		if v.Category == "env" {
			jv.Category = v1.VariableCategory_VARIABLE_CATEGORY_ENV
		}
		if v.Value != nil {
			jv.Value = *v.Value
		} else {
			plain, err := s.KEK.Open(ctx, tx, orgID, v.Sealed)
			if err != nil {
				return nil, fmt.Errorf("open variable %s: %w", v.Key, err)
			}
			jv.Value = string(plain)
		}
		job.Variables = append(job.Variables, jv)
	}

	var planRole, applyRole, region *string
	if err := tx.QueryRow(ctx, `SELECT a.aws_plan_role_arn, a.aws_apply_role_arn, a.aws_region FROM workspaces w
		LEFT JOIN cloud_accounts a ON a.id = w.cloud_account_id WHERE w.id = $1`, workspaceID).Scan(&planRole, &applyRole, &region); err != nil {
		return nil, err
	}
	if planRole != nil {
		phase, role := "plan", *planRole
		if operation == "apply" {
			phase, role = "apply", *applyRole
		}
		c := oidc.Claims{OrgID: orgID, OrgSlug: orgSlug, ProjectID: projectID, ProjectSlug: project,
			WorkspaceID: workspaceID, WorkspaceName: workspace, JobID: jobID, Phase: phase}
		if runID != nil {
			c.RunID = *runID
		}
		token, err := s.Issuer.Mint(c, oidc.AWSAudience)
		if err != nil {
			return nil, err
		}
		job.Aws = &v1.AwsCredentials{RoleArn: role, Region: *region, WebIdentityToken: token, StsEndpoint: s.STSEndpoint}
	}
	return job, nil
}

// ReadArtifact returns an artifact that a job uploaded. tx needs the org
// setting.
func (s *Service) ReadArtifact(ctx context.Context, tx pgx.Tx, jobID, name string) ([]byte, error) {
	var orgID string
	var sealedKey []byte
	if err := tx.QueryRow(ctx, `SELECT org_id, artifact_key FROM jobs WHERE id = $1`, jobID).Scan(&orgID, &sealedKey); err != nil {
		return nil, err
	}
	key, err := s.KEK.Open(ctx, tx, orgID, sealedKey)
	if err != nil {
		return nil, err
	}
	obj, err := s.S3.GetObject(ctx, s.Bucket, objectKey(orgID, jobID, name), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = obj.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(obj); minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return nil, fmt.Errorf("%w: %s", ErrNoArtifact, name)
	} else if err != nil {
		return nil, err
	}
	return artifact.Open(key, jobID, name, buf.Bytes())
}

// Finish ends a job and releases any state lock it holds. tx needs the org
// setting.
func (s *Service) Finish(ctx context.Context, tx pgx.Tx, jobID string, succeeded bool, msg string, hasChanges bool) error {
	status := "failed"
	if succeeded {
		status = "succeeded"
	}
	return s.end(ctx, tx, jobID, status, msg, &hasChanges)
}

func (s *Service) end(ctx context.Context, tx pgx.Tx, jobID, status, msg string, hasChanges *bool) error {
	tag, err := tx.Exec(ctx, `UPDATE jobs SET status = $2, error = nullif($3, ''), has_changes = $4, finished_at = now()
		WHERE id = $1 AND status IN ('queued', 'running')`, jobID, status, msg, hasChanges)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM state_locks WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM job_leases WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	if tag.RowsAffected() == 0 || s.OnEnd == nil {
		return nil
	}
	return s.OnEnd(ctx, tx, jobID, status, msg)
}

// Abandon fails a job that its sandbox can no longer finish, such as one
// whose sandbox exited first. A job that already ended is left alone.
func (s *Service) Abandon(ctx context.Context, jobID, msg string) error {
	var orgID string
	err := s.Pool.QueryRow(ctx, `SELECT org_id FROM job_leases WHERE job_id = $1`, jobID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.inOrg(ctx, orgID, func(tx pgx.Tx) error { return s.end(ctx, tx, jobID, "failed", msg, nil) })
}

// Event is one line an executor reported.
type Event struct {
	Seq          int64
	Type         string
	ResourceAddr string
	Payload      string
	At           time.Time
}

// RecordEvents stores events that are not stored yet. A job in a run also
// gives each event its place in the run's timeline. It returns the run, or
// "". tx needs the org setting.
func RecordEvents(ctx context.Context, tx pgx.Tx, jobID, orgID string, events []Event) (string, error) {
	var runID *string
	if err := tx.QueryRow(ctx, `SELECT run_id FROM jobs WHERE id = $1`, jobID).Scan(&runID); err != nil {
		return "", err
	}
	var last *int64
	if runID != nil {
		var l int64
		if err := tx.QueryRow(ctx, `SELECT last_seq FROM runs WHERE id = $1 FOR UPDATE`, *runID).Scan(&l); err != nil {
			return "", err
		}
		last = &l
	}
	n := len(events)
	seqs, types, addrs, payloads, ats := make([]int64, n), make([]string, n), make([]string, n), make([]string, n), make([]time.Time, n)
	for i, e := range events {
		seqs[i], types[i], addrs[i], payloads[i], ats[i] = e.Seq, e.Type, e.ResourceAddr, e.Payload, e.At
	}
	tag, err := tx.Exec(ctx, `INSERT INTO job_events (job_id, org_id, seq, type, resource_addr, payload, at, run_seq)
		SELECT $1, $2, e.seq, e.type, e.addr, e.payload::jsonb, e.at, $8::bigint + row_number() OVER (ORDER BY e.seq)
		FROM unnest($3::bigint[], $4::text[], $5::text[], $6::text[], $7::timestamptz[]) AS e (seq, type, addr, payload, at)
		WHERE NOT EXISTS (SELECT FROM job_events x WHERE x.job_id = $1 AND x.seq = e.seq)`,
		jobID, orgID, seqs, types, addrs, payloads, ats, last)
	if err != nil || runID == nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return *runID, nil
	}
	_, err = tx.Exec(ctx, `UPDATE runs SET last_seq = last_seq + $2, updated_at = now() WHERE id = $1`, *runID, tag.RowsAffected())
	return *runID, err
}

// Reap ends the jobs whose sandbox stopped reporting, was never claimed, or
// ran past its time limit.
func (s *Service) Reap(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT job_id, org_id,
		CASE WHEN expires_at < now() THEN 'the job ran past its time limit'
		     WHEN token_hash IS NULL THEN 'no sandbox claimed the job'
		     ELSE 'the sandbox stopped reporting' END
		FROM job_leases
		WHERE expires_at < now()
		   OR (token_hash IS NULL AND created_at < now() - make_interval(secs => $1))
		   OR (token_hash IS NOT NULL AND heartbeat_at < now() - make_interval(secs => $2))`,
		unclaimedFor.Seconds(), lostAfter.Seconds())
	if err != nil {
		return err
	}
	type lost struct{ job, org, why string }
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lost, error) {
		var l lost
		return l, r.Scan(&l.job, &l.org, &l.why)
	})
	if err != nil {
		return err
	}
	for _, l := range all {
		if err := s.inOrg(ctx, l.org, func(tx pgx.Tx) error { return s.end(ctx, tx, l.job, "lost", l.why, nil) }); err != nil {
			return err
		}
		slog.Info("job lost", "job", l.job, "reason", l.why)
	}
	return nil
}

func (s *Service) RunReaper(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := s.Reap(ctx); err != nil && ctx.Err() == nil {
			slog.Error("reaping lost jobs", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
