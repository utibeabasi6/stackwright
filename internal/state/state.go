package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/stackwright/stackwright/internal/seal"
)

const MaxSize = 64 << 20

var (
	ErrNotJSON      = errors.New("the state is not valid JSON")
	ErrLineage      = errors.New("the state's lineage differs from the stored state; it belongs to another workspace")
	ErrSerialBehind = errors.New("the state's serial is not newer than the stored state")
)

type Store struct {
	S3     *minio.Client
	Bucket string
	KEK    seal.KEK
}

type Version struct {
	ID        string
	Serial    int64
	Lineage   string
	SHA256    []byte
	Resources int
	BlobKey   string
}

type document struct {
	Serial    int64             `json:"serial"`
	Lineage   string            `json:"lineage"`
	Resources []json.RawMessage `json:"resources"`
}

func parse(body []byte) (document, error) {
	var d document
	if err := json.Unmarshal(body, &d); err != nil {
		return document{}, ErrNotJSON
	}
	return d, nil
}

// tx needs the org setting.
func Latest(ctx context.Context, tx pgx.Tx, workspaceID string) (Version, bool, error) {
	var v Version
	err := tx.QueryRow(ctx, `SELECT id, serial, lineage, sha256, resources, blob_key FROM state_versions
		WHERE workspace_id = $1 ORDER BY serial DESC LIMIT 1`, workspaceID).
		Scan(&v.ID, &v.Serial, &v.Lineage, &v.SHA256, &v.Resources, &v.BlobKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, false, nil
	}
	return v, err == nil, err
}

// Write refuses a lineage change and a serial that does not move forward.
// The same serial with the same bytes is a no-op.
func (s Store) Write(ctx context.Context, tx pgx.Tx, orgID, workspaceID, userID string, body []byte) (Version, error) {
	d, err := parse(body)
	if err != nil {
		return Version{}, err
	}
	sum := sha256.Sum256(body)
	cur, ok, err := Latest(ctx, tx, workspaceID)
	if err != nil {
		return Version{}, err
	}
	if ok {
		switch {
		case d.Lineage != cur.Lineage:
			return Version{}, ErrLineage
		case d.Serial == cur.Serial && bytes.Equal(sum[:], cur.SHA256):
			return cur, nil
		case d.Serial <= cur.Serial:
			return Version{}, ErrSerialBehind
		}
	}
	return s.insert(ctx, tx, orgID, workspaceID, userID, d, body, "")
}

func (s Store) insert(ctx context.Context, tx pgx.Tx, orgID, workspaceID, userID string, d document, body []byte, rolledBackFrom string) (Version, error) {
	sealed, err := s.KEK.Seal(ctx, tx, orgID, body)
	if err != nil {
		return Version{}, err
	}
	sum := sha256.Sum256(body)
	v := Version{ID: uuid.NewString(), Serial: d.Serial, Lineage: d.Lineage, SHA256: sum[:], Resources: len(d.Resources)}
	v.BlobKey = fmt.Sprintf("orgs/%s/state/%s/%s", orgID, workspaceID, v.ID)
	if _, err := s.S3.PutObject(ctx, s.Bucket, v.BlobKey, bytes.NewReader(sealed), int64(len(sealed)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return Version{}, fmt.Errorf("store state blob: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO state_versions (id, org_id, workspace_id, serial, lineage, sha256, size, resources, blob_key, created_by, rolled_back_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, '')::uuid, nullif($11, '')::uuid)`,
		v.ID, orgID, workspaceID, v.Serial, v.Lineage, v.SHA256, len(body), v.Resources, v.BlobKey, userID, rolledBackFrom)
	if err != nil {
		return Version{}, fmt.Errorf("record state version: %w", err)
	}
	return v, nil
}

func (s Store) Read(ctx context.Context, tx pgx.Tx, orgID, blobKey string) ([]byte, error) {
	obj, err := s.S3.GetObject(ctx, s.Bucket, blobKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetch state blob: %w", err)
	}
	defer func() { _ = obj.Close() }()
	sealed, err := io.ReadAll(io.LimitReader(obj, MaxSize+1024))
	if err != nil {
		return nil, fmt.Errorf("fetch state blob: %w", err)
	}
	return s.KEK.Open(ctx, tx, orgID, sealed)
}

// Rollback writes a copy of an old version as the newest one, with the next
// serial, so the engine's next write moves forward from it.
func (s Store) Rollback(ctx context.Context, tx pgx.Tx, orgID, workspaceID, userID, versionID string) (Version, error) {
	var blobKey string
	if err := tx.QueryRow(ctx, `SELECT blob_key FROM state_versions WHERE id = $1 AND workspace_id = $2`,
		versionID, workspaceID).Scan(&blobKey); err != nil {
		return Version{}, err
	}
	old, err := s.Read(ctx, tx, orgID, blobKey)
	if err != nil {
		return Version{}, err
	}
	cur, _, err := Latest(ctx, tx, workspaceID)
	if err != nil {
		return Version{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(old, &raw); err != nil {
		return Version{}, ErrNotJSON
	}
	raw["serial"] = json.RawMessage(fmt.Sprint(cur.Serial + 1))
	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return Version{}, err
	}
	d, err := parse(body)
	if err != nil {
		return Version{}, err
	}
	return s.insert(ctx, tx, orgID, workspaceID, userID, d, body, versionID)
}
