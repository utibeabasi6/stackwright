package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// apiVersion is the oldest Docker Engine API that Docker 29 accepts.
const apiVersion = "v1.44"

const (
	user     = "65532:65532"
	uid      = 65532
	cpus     = 2
	memory   = 2 << 30
	pids     = 1024
	workSize = "2g"
	tmpSize  = "256m"
)

var ErrNoSuchImage = errors.New("the sandbox image is not on the Docker host")

// Docker runs each sandbox as one container on a bridge network of its own,
// through the Docker Engine API on a Unix socket.
type Docker struct {
	client     *http.Client
	extraHosts []string
}

// NewDocker talks to host, a unix:// address. An empty host means
// DOCKER_HOST, then /var/run/docker.sock. extraHosts are added to each
// sandbox's /etc/hosts, such as host.docker.internal:host-gateway.
func NewDocker(host string, extraHosts ...string) (*Docker, error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		return nil, fmt.Errorf("docker host %q: only unix:// sockets are supported", host)
	}
	return &Docker{
		client: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}},
		extraHosts: extraHosts,
	}, nil
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("docker: %d %s", e.status, e.msg) }

func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusNotFound
}

func (d *Docker) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := "http://docker/" + apiVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		var e struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		return nil, &apiError{status: resp.StatusCode, msg: e.Message}
	}
	return resp, nil
}

func (d *Docker) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, contentType = bytes.NewReader(b), "application/json"
	}
	resp, err := d.do(ctx, method, path, query, body, contentType)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (d *Docker) Platform(ctx context.Context) (string, error) {
	var v struct{ Os, Arch string }
	if err := d.call(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return "", err
	}
	return v.Os + "_" + v.Arch, nil
}

func (d *Docker) Start(ctx context.Context, spec Spec) (Sandbox, error) {
	name := "stackwright-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	labels := map[string]string{"dev.stackwright.sandbox": name}
	maps.Copy(labels, spec.Labels)
	s := &dockerSandbox{d: d, name: name}
	ok := false
	defer func() {
		if !ok {
			_ = s.Remove(context.WithoutCancel(ctx))
		}
	}()

	var network struct{ ID string }
	if err := d.call(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "Labels": labels,
	}, &network); err != nil {
		return nil, fmt.Errorf("create sandbox network: %w", err)
	}
	s.networkID = network.ID

	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	var container struct{ ID string }
	err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, map[string]any{
		"Image":      spec.Image,
		"Cmd":        spec.Command,
		"Env":        env,
		"User":       user,
		"WorkingDir": "/work",
		"Labels":     labels,
		"HostConfig": map[string]any{
			"NetworkMode":    name,
			"ReadonlyRootfs": true,
			"Tmpfs": map[string]string{
				"/work": "rw,exec,nosuid,nodev,size=" + workSize + ",mode=1777",
				"/tmp":  "rw,nosuid,nodev,size=" + tmpSize + ",mode=1777",
			},
			"Mounts":      []map[string]any{{"Type": "volume", "Target": FilesDir}},
			"CapDrop":     []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges"},
			"NanoCpus":    cpus * 1e9,
			"Memory":      memory,
			"MemorySwap":  memory,
			"PidsLimit":   pids,
			"ExtraHosts":  d.extraHosts,
		},
	}, &container)
	if isNotFound(err) {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchImage, spec.Image)
	}
	if err != nil {
		return nil, fmt.Errorf("create sandbox container: %w", err)
	}
	s.containerID = container.ID

	if len(spec.Files) > 0 {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, n := range slices.Sorted(maps.Keys(spec.Files)) {
			data := spec.Files[n]
			if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o400, Size: int64(len(data)), Uid: uid, Gid: uid, ModTime: time.Now()}); err != nil {
				return nil, err
			}
			_, _ = tw.Write(data)
		}
		if err := tw.Close(); err != nil {
			return nil, err
		}
		resp, err := d.do(ctx, http.MethodPut, "/containers/"+s.containerID+"/archive", url.Values{"path": {FilesDir}}, &buf, "application/x-tar")
		if err != nil {
			return nil, fmt.Errorf("copy files into the sandbox: %w", err)
		}
		_ = resp.Body.Close()
	}

	if err := d.call(ctx, http.MethodPost, "/containers/"+s.containerID+"/start", nil, nil, nil); err != nil {
		return nil, fmt.Errorf("start sandbox: %w", err)
	}
	ok = true
	return s, nil
}

type dockerSandbox struct {
	d           *Docker
	name        string
	containerID string
	networkID   string
}

func (s *dockerSandbox) ID() string { return s.name }

func (s *dockerSandbox) Wait(ctx context.Context) (int, error) {
	var r struct{ StatusCode int }
	if err := s.d.call(ctx, http.MethodPost, "/containers/"+s.containerID+"/wait", url.Values{"condition": {"not-running"}}, nil, &r); err != nil {
		return -1, err
	}
	return r.StatusCode, nil
}

// Logs joins stdout and stderr. Without a TTY, Docker frames each chunk
// with an 8-byte header whose last 4 bytes are the length:
// https://docs.docker.com/reference/api/engine/version/v1.44/#tag/Container/operation/ContainerAttach
func (s *dockerSandbox) Logs(ctx context.Context) ([]byte, error) {
	resp, err := s.d.do(ctx, http.MethodGet, "/containers/"+s.containerID+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}}, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(resp.Body, header); errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		} else if err != nil {
			return out.Bytes(), err
		}
		if _, err := io.CopyN(&out, resp.Body, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return out.Bytes(), err
		}
	}
}

func (s *dockerSandbox) Remove(ctx context.Context) error {
	var errs []error
	if s.containerID != "" {
		err := s.d.call(ctx, http.MethodDelete, "/containers/"+s.containerID, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
		if err != nil && !isNotFound(err) {
			errs = append(errs, err)
		}
	}
	if s.networkID != "" {
		err := s.d.call(ctx, http.MethodDelete, "/networks/"+s.networkID, nil, nil, nil)
		if err != nil && !isNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
