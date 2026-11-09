package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Pipeline struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url,omitempty"`
	SHA    string `json:"sha,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

type apiClient struct {
	base, project, token string
	http                 *http.Client
}

var errInvalidJSON = errors.New("invalid GitLab JSON response")

func (a *app) client() (*apiClient, error) {
	token := strings.TrimSpace(os.Getenv("GITLAB_TOKEN"))
	if a.tokenFile != "" {
		b, err := os.ReadFile(a.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read token file")
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, fmt.Errorf("set GITLAB_TOKEN or --token-file")
	}
	if err := validateGitLabURL(a.base); err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(a.base)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := http.Client{Transport: transport}
	if a.opts.HTTPClient != nil {
		client = *a.opts.HTTPClient
	}
	prior := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if via[0].Method != "GET" {
			return http.ErrUseLastResponse
		}
		if prior != nil {
			if err := prior(req, via); err != nil {
				return err
			}
		}
		if req.URL.Scheme != parsed.Scheme || req.URL.Host != parsed.Host {
			req.Header.Del("PRIVATE-TOKEN")
		}
		return nil
	}
	return &apiClient{strings.TrimRight(a.base, "/"), a.project, token, &client}, nil
}

func validateGitLabURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" || parsed.Hostname() == "::1"))) {
		return fmt.Errorf(
			"GitLab URL must use HTTPS and contain no credentials, query or fragment (HTTP allowed only on loopback)",
		)
	}
	return nil
}

func (c *apiClient) json(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		c.base+"/api/v4/projects/"+url.PathEscape(c.project)+path,
		body,
	)
	if err != nil {
		return fmt.Errorf("invalid API URL")
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil {
		return nil
	}
	limit := int64(16 << 20)
	if strings.HasSuffix(path, "/artifacts/operation-result.json") {
		limit = 64 << 20
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(output); err != nil {
		return errInvalidJSON
	}
	return nil
}

type apiError struct {
	method string
	status int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitLab %s: HTTP %d", e.method, e.status)
}

func (c *apiClient) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("GitLab request timed out: %w", context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("GitLab request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &apiError{req.Method, resp.StatusCode}
	}
	return resp, nil
}

func (c *apiClient) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		"GET",
		c.base+"/api/v4/projects/"+url.PathEscape(c.project)+path,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid API URL")
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	return c.do(req)
}

func (c *apiClient) validateProductionRef(ctx context.Context, ref string) error {
	var tag struct {
		Protected bool `json:"protected"`
		Commit    struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.json(ctx, "GET", "/repository/tags/"+url.PathEscape(ref), nil, &tag); err != nil {
		return fmt.Errorf("cannot verify production tag: %w", err)
	}
	if !tag.Protected || tag.Commit.ID == "" {
		return fmt.Errorf("production tag must be protected")
	}
	var base struct {
		ID string `json:"id"`
	}
	values := url.Values{"refs[]": []string{tag.Commit.ID, "main"}}
	if err := c.json(ctx, "GET", "/repository/merge_base?"+values.Encode(), nil, &base); err != nil {
		return fmt.Errorf("cannot verify production tag ancestry: %w", err)
	}
	if base.ID != tag.Commit.ID {
		return fmt.Errorf("production tag must be ancestral to main")
	}
	return nil
}
