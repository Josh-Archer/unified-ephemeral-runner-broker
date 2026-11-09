package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SetRepositoryVariable creates or updates a repository Actions variable.
// It needs the GitHub App "Variables: write" repository permission.
func (c *Client) SetRepositoryVariable(ctx context.Context, repository, name, value string) error {
	base, err := repositoryPath(repository)
	if err != nil {
		return err
	}
	body := map[string]string{"name": name, "value": value}
	err = c.do(ctx, http.MethodPatch, base+"/actions/variables/"+url.PathEscape(name), body, nil, http.StatusNoContent)
	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return c.do(ctx, http.MethodPost, base+"/actions/variables", body, nil, http.StatusCreated)
	}
	return err
}

// RepositoryIsPrivate reports whether GitHub bills Actions minutes for the
// repository (private and internal repositories); public ones are free on
// standard GitHub-hosted runners.
func (c *Client) RepositoryIsPrivate(ctx context.Context, repository string) (bool, error) {
	base, err := repositoryPath(repository)
	if err != nil {
		return false, err
	}
	var out struct {
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	if err := c.do(ctx, http.MethodGet, base, nil, &out, http.StatusOK); err != nil {
		return false, err
	}
	return out.Private || !strings.EqualFold(out.Visibility, "public"), nil
}
