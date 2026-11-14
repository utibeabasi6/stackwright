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
	cpus     = 2
	memory   = 4 << 30
	pids     = 1024
	workSize = "3g"
	tmpSize  = "256m"
)

var ErrNoSuchImage = errors.New("the sandbox image is not on the Docker host")

// Docker runs each sandbox as one container on a bridge network of its own,
// through the Docker Engine API on a Unix socket.
type Docker struct {
	client *http.Client
	c      DockerConfig
}

type DockerConfig struct {
	// Host is a unix:// address. Empty means DOCKER_HOST, then
	// /var/run/docker.sock.
	Host string
	// ExtraHosts are added to each sandbox's /etc/hosts, such as
	// host.docker.internal:host-gateway.
	ExtraHosts []string
	// Join names services of the compose project that the caller runs in.
	// Each one joins every sandbox's network, and the sandbox reaches it by
	// its service name.
	Join []string
	// Cache is the builder's cache as Docker mounts it: a host path, or a
	// volume name.
	Cache string
}

func NewDocker(c DockerConfig) (*Docker, error) {
	host := c.Host
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
		c: c,
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
	req, err := http.NewRequestWithContext(ctx, method, u, body) //nolint:gosec // the client dials only the Docker socket
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := d.client.Do(req) //nolint:gosec // the client dials only the Docker socket
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
	if err := s.join(ctx); err != nil {
		return nil, err
	}

	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	mounts := []map[string]any{{"Type": "volume", "Target": FilesDir}}
	if spec.Cache != NoCache {
		if d.c.Cache == "" {
			return nil, errors.New("this builder has no cache")
		}
		typ := "volume"
		if strings.HasPrefix(d.c.Cache, "/") {
			typ = "bind"
		}
		mounts = append(mounts, map[string]any{"Type": typ, "Source": d.c.Cache, "Target": CacheDir, "ReadOnly": spec.Cache == CacheReadOnly})
	}
	create := map[string]any{
		"Image":      spec.Image,
		"Cmd":        spec.Command,
		"Env":        env,
		"User":       fmt.Sprintf("%d:%d", UID, UID),
		"WorkingDir": "/work",
		"Labels":     labels,
		"HostConfig": map[string]any{
			"NetworkMode":    name,
			"ReadonlyRootfs": true,
			"Tmpfs": map[string]string{
				"/work": "rw,exec,nosuid,nodev,size=" + workSize + ",mode=1777",
				"/tmp":  "rw,nosuid,nodev,size=" + tmpSize + ",mode=1777",
			},
			"Mounts":      mounts,
			"CapDrop":     []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges"},
			"NanoCpus":    cpus * 1e9,
			"Memory":      memory,
			"MemorySwap":  memory,
			"PidsLimit":   pids,
			"ExtraHosts":  d.c.ExtraHosts,
		},
	}
	var container struct{ ID string }
	err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, create, &container)
	if isNotFound(err) {
		if err := d.pull(ctx, spec.Image); err != nil {
			return nil, err
		}
		err = d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, create, &container)
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
			if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o400, Size: int64(len(data)), Uid: UID, Gid: UID, ModTime: time.Now()}); err != nil {
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

// pull fetches image. Docker streams progress as JSON lines, and reports a
// failure in a line's error field with status 200.
func (d *Docker) pull(ctx context.Context, image string) error {
	resp, err := d.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {image}}, nil, "")
	if isNotFound(err) {
		return fmt.Errorf("%w: %s", ErrNoSuchImage, image)
	}
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	defer func() { _ = resp.Body.Close() }()
	dec := json.NewDecoder(resp.Body)
	for {
		var line struct{ Error string }
		if err := dec.Decode(&line); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("pull %s: %w", image, err)
		}
		if line.Error != "" {
			return fmt.Errorf("pull %s: %s", image, line.Error)
		}
	}
}

type dockerSandbox struct {
	d           *Docker
	name        string
	containerID string
	networkID   string
	joined      []string
}

func (s *dockerSandbox) ID() string { return s.name }

// join finds each of the caller's compose services by its labels, so
// recreated containers are found too.
func (s *dockerSandbox) join(ctx context.Context) error {
	if len(s.d.c.Join) == 0 {
		return nil
	}
	var self struct {
		Config struct{ Labels map[string]string }
	}
	if err := s.d.call(ctx, http.MethodGet, "/containers/"+os.Getenv("HOSTNAME")+"/json", nil, nil, &self); err != nil {
		return fmt.Errorf("find this container's compose project: %w", err)
	}
	project := self.Config.Labels["com.docker.compose.project"]
	if project == "" {
		return errors.New("joining services needs the builder to run in compose")
	}
	for _, service := range s.d.c.Join {
		filters, _ := json.Marshal(map[string][]string{"label": {
			"com.docker.compose.project=" + project, "com.docker.compose.service=" + service,
		}})
		var found []struct{ ID string }
		if err := s.d.call(ctx, http.MethodGet, "/containers/json", url.Values{"filters": {string(filters)}}, nil, &found); err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("no running %s container in compose project %s", service, project)
		}
		for _, c := range found {
			if err := s.d.call(ctx, http.MethodPost, "/networks/"+s.networkID+"/connect", nil, map[string]any{
				"Container": c.ID, "EndpointConfig": map[string]any{"Aliases": []string{service}},
			}, nil); err != nil {
				return fmt.Errorf("join %s to the sandbox network: %w", service, err)
			}
			s.joined = append(s.joined, c.ID)
		}
	}
	return nil
}

// Stop asks the sandbox to stop and kills it after grace.
func (s *dockerSandbox) Stop(ctx context.Context, grace time.Duration) error {
	err := s.d.call(ctx, http.MethodPost, "/containers/"+s.containerID+"/stop", url.Values{"t": {fmt.Sprint(int(grace.Seconds()))}}, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (s *dockerSandbox) Wait(ctx context.Context) (int32, error) {
	var r struct{ StatusCode int32 }
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
	for _, c := range s.joined {
		err := s.d.call(ctx, http.MethodPost, "/networks/"+s.networkID+"/disconnect", nil, map[string]any{"Container": c, "Force": true}, nil)
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
