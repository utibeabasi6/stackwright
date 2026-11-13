package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/stackwright/stackwright/internal/importer"
)

const (
	maxArchive    = 64 << 20
	maxConfigFile = 1 << 20
)

var ErrArchiveTooLarge = fmt.Errorf("the commit's archive is larger than %d MiB", maxArchive>>20)

type repoRow struct {
	id            string
	installation  int64
	fullName      string
	defaultBranch string
}

type archive struct {
	key    string
	sha256 string
	// files holds the commit's configuration files by path, for the module
	// walker and the importer's scan.
	files map[string][]byte
	// names lists every file of the commit.
	names []string
}

// archive stores one sealed copy of the commit, as a .tar.gz with no top
// folder, like an upload. tx needs the org setting.
func (g *GitHub) archive(ctx context.Context, tx pgx.Tx, orgID string, repo repoRow, commit string) (archive, error) {
	token, err := g.installationToken(ctx, repo.installation)
	if err != nil {
		return archive{}, err
	}
	resp, err := g.request(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/tarball/%s", g.APIURL, repo.fullName, commit), token, nil)
	if err != nil {
		return archive{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return archive{}, err
	}
	if len(body) > maxArchive {
		return archive{}, ErrArchiveTooLarge
	}
	packed, files, names, err := repack(body)
	if err != nil {
		return archive{}, fmt.Errorf("read the archive of %s at %s: %w", repo.fullName, commit, err)
	}
	sum := sha256.Sum256(packed)
	a := archive{key: fmt.Sprintf("orgs/%s/sources/%s/%s.tar.gz", orgID, repo.id, commit), sha256: hex.EncodeToString(sum[:]), files: files, names: names}
	var stored string
	err = tx.QueryRow(ctx, `SELECT sha256 FROM source_archives WHERE repo_id = $1 AND commit_sha = $2`, repo.id, commit).Scan(&stored)
	if err == nil && stored == a.sha256 {
		return a, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return archive{}, err
	}
	sealed, err := g.KEK.Seal(ctx, tx, orgID, packed)
	if err != nil {
		return archive{}, err
	}
	if _, err := g.S3.PutObject(ctx, g.Bucket, a.key, bytes.NewReader(sealed), int64(len(sealed)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return archive{}, fmt.Errorf("store the archive: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO source_archives (org_id, repo_id, commit_sha, object_key, sha256) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (repo_id, commit_sha) DO NOTHING`, orgID, repo.id, commit, a.key, a.sha256)
	return a, err
}

// repack drops the top folder that GitHub puts every file in, and GitHub's
// pax global header, which holds the commit id.
func repack(src []byte) ([]byte, map[string][]byte, []string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, nil, nil, err
	}
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	files := map[string][]byte{}
	var names []string
	total := int64(0)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		_, name, ok := strings.Cut(h.Name, "/")
		if !ok || name == "" {
			continue
		}
		total += h.Size
		if total > 4*maxArchive {
			return nil, nil, nil, ErrArchiveTooLarge
		}
		h.Name = name
		if err := tw.WriteHeader(h); err != nil {
			return nil, nil, nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		names = append(names, name)
		var buf bytes.Buffer
		w := io.Writer(tw)
		keep := importer.IsScanned(name) && h.Size <= maxConfigFile
		if keep {
			w = io.MultiWriter(tw, &buf)
		}
		if _, err := io.CopyN(w, tr, h.Size); err != nil {
			return nil, nil, nil, err
		}
		if keep {
			files[name] = buf.Bytes()
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, nil, nil, err
	}
	return out.Bytes(), files, names, nil
}

// RepositoryHead reads a linked repository at its default branch's newest
// commit, for the importer's scan: the commit, every file's name, and the
// files the scan reads. tx needs the org setting.
func (g *GitHub) RepositoryHead(ctx context.Context, tx pgx.Tx, orgID, fullName string) (string, []string, map[string][]byte, error) {
	var repo repoRow
	if err := tx.QueryRow(ctx, `SELECT id, installation_id, full_name, default_branch FROM repos WHERE lower(full_name) = lower($1)`, fullName).
		Scan(&repo.id, &repo.installation, &repo.fullName, &repo.defaultBranch); err != nil {
		return "", nil, nil, err
	}
	token, err := g.installationToken(ctx, repo.installation)
	if err != nil {
		return "", nil, nil, err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := g.call(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s", repo.fullName, url.PathEscape(repo.defaultBranch)), token, nil, &commit); err != nil {
		return "", nil, nil, fmt.Errorf("find the newest commit on %s: %w", repo.defaultBranch, err)
	}
	a, err := g.archive(ctx, tx, orgID, repo, commit.SHA)
	if err != nil {
		return "", nil, nil, err
	}
	return commit.SHA, a.names, a.files, nil
}
