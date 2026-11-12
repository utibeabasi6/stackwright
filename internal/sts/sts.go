// Package sts trades a web identity token for AWS credentials with
// AssumeRoleWithWebIdentity. The call is unsigned: the token is the proof.
// https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRoleWithWebIdentity.html
package sts

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrNoCredentials = errors.New("STS answered with no credentials")

type Credentials struct {
	AccessKeyID     string    `xml:"AccessKeyId"`
	SecretAccessKey string    `xml:"SecretAccessKey"`
	SessionToken    string    `xml:"SessionToken"`
	Expiration      time.Time `xml:"Expiration"`
}

// Exchange returns credentials for roleARN. session names the session in
// CloudTrail, so each job can be traced.
func Exchange(ctx context.Context, endpoint, roleARN, session, token string, duration time.Duration) (Credentials, error) {
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {roleARN},
		"RoleSessionName":  {session},
		"WebIdentityToken": {token},
		"DurationSeconds":  {fmt.Sprint(int(duration.Seconds()))},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Credentials{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Credentials{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Code, Message string } `xml:"Error"`
		}
		_ = xml.Unmarshal(body, &e)
		return Credentials{}, fmt.Errorf("assume %s with a web identity: %s: %s %s", roleARN, resp.Status, e.Error.Code, e.Error.Message)
	}
	var ok struct {
		Result struct {
			Credentials Credentials `xml:"Credentials"`
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	if err := xml.Unmarshal(body, &ok); err != nil {
		return Credentials{}, err
	}
	if ok.Result.Credentials.AccessKeyID == "" {
		return Credentials{}, ErrNoCredentials
	}
	return ok.Result.Credentials, nil
}
