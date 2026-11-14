package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

// runLogin signs the CLI in with the OAuth device flow (adr/0044).
// https://www.rfc-editor.org/rfc/rfc8628
func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "", "the Stackwright URL, such as https://stackwright.example.com")
	host, _ := os.Hostname()
	name := fs.String("name", host, "the name this machine shows in the web app")
	_ = fs.Parse(args)
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *server != "" {
		c.Server = strings.TrimSuffix(*server, "/")
	}
	if c.Server == "" {
		return errors.New("name the server with -server")
	}
	ctx := context.Background()

	var code struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		URI        string `json:"verification_uri_complete"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	if status, err := postForm(ctx, c.Server+"/cli/device/code", url.Values{"name": {*name}}, &code); err != nil || status != http.StatusOK {
		return fmt.Errorf("start the sign-in: %d %w", status, err)
	}
	fmt.Printf("Open %s\nin a browser where you are signed in, and check that it shows the code %s.\n", code.URI, code.UserCode)

	interval := time.Duration(code.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		var got struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		if _, err := postForm(ctx, c.Server+"/cli/device/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {code.DeviceCode}}, &got); err != nil {
			return err
		}
		switch got.Error {
		case "":
			c.Token = got.AccessToken
			if err := saveConfig(c); err != nil {
				return err
			}
			a, err := signedIn()
			if err != nil {
				return err
			}
			me, err := a.auth.GetMe(ctx, connect.NewRequest(&v1.GetMeRequest{}))
			if err != nil {
				return err
			}
			fmt.Printf("Signed in to %s as %s.\n", c.Server, me.Msg.GetUser().GetEmail())
			return nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return errors.New("the sign-in was denied in the browser")
		default:
			return fmt.Errorf("the sign-in ended: %s", got.Error)
		}
	}
	return errors.New("the code ended before anyone approved it; run stackwright login again")
}

func postForm(ctx context.Context, u string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

func saveConfig(c cliConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
