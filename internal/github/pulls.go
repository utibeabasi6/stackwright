package github

import (
	"context"
	"net/http"
)

// OpenPullRequest opens a pull request, ready for review, from head to
// base, and returns its number and web address.
// https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request
func (g *GitHub) OpenPullRequest(ctx context.Context, installation int64, repo, head, base, title, body string) (int, string, error) {
	token, err := g.installationToken(ctx, installation)
	if err != nil {
		return 0, "", err
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	err = g.call(ctx, http.MethodPost, "/repos/"+repo+"/pulls", token,
		map[string]any{"title": title, "body": body, "head": head, "base": base, "draft": false}, &out)
	return out.Number, out.HTMLURL, err
}
