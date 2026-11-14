package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
)

// cliConfig is what `stackwright login` keeps, in configPath, mode 0600
// (adr/0044). STACKWRIGHT_SERVER and STACKWRIGHT_TOKEN override it.
type cliConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "stackwright", "config.json"), nil
}

func loadConfig() (cliConfig, error) {
	var c cliConfig
	path, err := configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(path) //nolint:gosec // the user's own config file
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	if s := os.Getenv("STACKWRIGHT_SERVER"); s != "" {
		c.Server = s
	}
	if t := os.Getenv("STACKWRIGHT_TOKEN"); t != "" {
		c.Token = t
	}
	c.Server = strings.TrimSuffix(c.Server, "/")
	return c, nil
}

// api is the Connect clients of a signed-in CLI.
type api struct {
	server   string
	auth     stackwrightv1connect.AuthServiceClient
	projects stackwrightv1connect.ProjectServiceClient
	runs     stackwrightv1connect.RunServiceClient
}

func signedIn() (*api, error) {
	c, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if c.Server == "" || c.Token == "" {
		return nil, errors.New("not signed in; run stackwright login -server https://your-stackwright")
	}
	opts := connect.WithClientOptions(connect.WithInterceptors(bearer(c.Token)))
	return &api{
		server:   c.Server,
		auth:     stackwrightv1connect.NewAuthServiceClient(http.DefaultClient, c.Server, opts),
		projects: stackwrightv1connect.NewProjectServiceClient(http.DefaultClient, c.Server, opts),
		runs:     stackwrightv1connect.NewRunServiceClient(http.DefaultClient, c.Server, opts),
	}, nil
}

// bearer sends the CLI token on every call.
type bearer string

func (t bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(t))
		return next(ctx, req)
	}
}

func (t bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(t))
		return conn
	}
}

func (bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// target is a workspace that a command acts on.
type target struct {
	org, project, workspace string
	// root is the repository's top folder, when git found one.
	root string
}

// targetFlags are the flags that name a workspace; with none, the CLI finds
// it from the git remote and the folder (adr/0044).
type targetFlags struct {
	org, project, workspace, dir *string
}

func addTargetFlags(fs *flag.FlagSet) targetFlags {
	return targetFlags{
		org:       fs.String("org", "", "the org; found from your memberships when you are in one"),
		project:   fs.String("project", "", "the project; found from the git remote and the folder"),
		workspace: fs.String("workspace", "", "the workspace; asked for when the project has several"),
		dir:       fs.String("dir", ".", "the folder to run in"),
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output() //nolint:gosec // git, with the user's own folder
	return strings.TrimSpace(string(out)), err
}

// repoOf turns a git remote URL into "owner/name".
func repoOf(remote string) string {
	r := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	if _, rest, ok := strings.Cut(r, "github.com"); ok {
		r = strings.TrimLeft(rest, ":/")
	}
	return strings.ToLower(r)
}

func (a *api) find(ctx context.Context, f targetFlags) (target, error) {
	t := target{org: *f.org, project: *f.project, workspace: *f.workspace}
	repo, prefix := "", ""
	if root, err := git(ctx, *f.dir, "rev-parse", "--show-toplevel"); err == nil {
		t.root = root
		prefix, _ = git(ctx, *f.dir, "rev-parse", "--show-prefix")
		prefix = strings.TrimSuffix(prefix, "/")
		remote, _ := git(ctx, *f.dir, "remote", "get-url", "origin")
		repo = repoOf(remote)
	}
	orgs := []string{t.org}
	if t.org == "" {
		me, err := a.auth.GetMe(ctx, connect.NewRequest(&v1.GetMeRequest{}))
		if err != nil {
			return t, err
		}
		orgs = nil
		for _, o := range me.Msg.GetOrgs() {
			orgs = append(orgs, o.GetSlug())
		}
	}
	type match struct {
		org string
		p   *v1.Project
	}
	var found []match
	for _, org := range orgs {
		ps, err := a.projects.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{Org: org}))
		if err != nil {
			return t, err
		}
		for _, p := range ps.Msg.GetProjects() {
			byName := t.project != "" && p.GetSlug() == t.project
			byGit := t.project == "" && repo != "" && strings.ToLower(p.GetLinkedRepo()) == repo && p.GetPath() == prefix
			if byName || byGit {
				found = append(found, match{org, p})
			}
		}
	}
	switch {
	case len(found) == 0 && t.project != "":
		return t, fmt.Errorf("no project %s in %s", t.project, strings.Join(orgs, ", "))
	case len(found) == 0:
		return t, fmt.Errorf("no project is linked to %q at %q; name one with -project", repo, prefix)
	case len(found) > 1:
		var names []string
		for _, m := range found {
			names = append(names, m.org+"/"+m.p.GetSlug())
		}
		return t, fmt.Errorf("several projects match: %s; name one with -org and -project", strings.Join(names, ", "))
	}
	t.org, t.project = found[0].org, found[0].p.GetSlug()
	var names []string
	for _, w := range found[0].p.GetWorkspaces() {
		names = append(names, w.GetName())
	}
	switch {
	case t.workspace != "":
	case len(names) == 1:
		t.workspace = names[0]
	case len(names) == 0:
		return t, fmt.Errorf("project %s has no workspaces", t.project)
	default:
		fi, err := os.Stdin.Stat()
		null, _ := os.Stat(os.DevNull)
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 || null != nil && os.SameFile(fi, null) {
			return t, fmt.Errorf("project %s has several workspaces: %s; name one with -workspace", t.project, strings.Join(names, ", "))
		}
		for i, n := range names {
			fmt.Fprintf(os.Stderr, "%d) %s\n", i+1, n)
		}
		fmt.Fprint(os.Stderr, "Workspace: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(names) {
			return t, errors.New("choose a workspace by its number")
		}
		t.workspace = names[n-1]
	}
	return t, nil
}

// follow prints a run's timeline, as the web app shows it, from after seq.
// With wait, it goes on until the run ends; it returns the run's last state.
func (a *api) follow(ctx context.Context, w io.Writer, t target, number int32, wait bool) (*v1.Run, error) {
	stream, err := a.runs.WatchRun(ctx, connect.NewRequest(&v1.WatchRunRequest{Org: t.org, Project: t.project, Workspace: t.workspace, Number: number}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var run *v1.Run
	var seq int64
	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetRun() != nil {
			run = msg.GetRun()
		}
		for _, e := range msg.GetEvents() {
			seq = e.GetSeq()
			if strings.TrimSpace(e.GetText()) == "" {
				continue
			}
			line := e.GetText()
			if e.GetResourceAddr() != "" && !strings.Contains(line, e.GetResourceAddr()) {
				line += " " + e.GetResourceAddr()
			}
			if d := e.GetDetail(); d != "" {
				line += "\n" + d
			}
			_, _ = fmt.Fprintf(w, "%s  %s\n", e.GetAt().AsTime().Local().Format(time.TimeOnly), strings.ReplaceAll(line, "\n", "\n          "))
		}
		if !wait && run != nil && seq >= run.GetLastSeq() {
			return run, nil
		}
	}
	return run, stream.Err()
}
