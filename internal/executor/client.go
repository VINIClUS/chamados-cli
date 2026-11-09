package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

// Settings is sent only through the SSH stdin channel, never journaled or logged.
type Settings struct {
	APIURL      string `json:"api_url"`
	ProjectID   string `json:"project_id"`
	ProjectPath string `json:"project_path"`
	PipelineIID string `json:"pipeline_iid"`
	Token       string `json:"job_token"`
}

type JobClient struct {
	settings Settings
	http     *http.Client
}

type HTTPError struct {
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("GitLab HTTP %d", e.Status)
}

func NewJobClient(s Settings) (*JobClient, error) {
	base, err := url.Parse(s.APIURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		!strings.HasSuffix(base.Path, "/api/v4") ||
		(base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "localhost" || base.Hostname() == "127.0.0.1" || base.Hostname() == "::1"))) {
		return nil, fmt.Errorf("invalid GitLab API URL")
	}
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(s.ProjectID) || strings.TrimSpace(s.Token) == "" ||
		strings.ContainsAny(s.Token, "\x00\r\n") {
		return nil, fmt.Errorf("invalid job credentials")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := &http.Client{Transport: transport, Timeout: 120 * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if via[0].Method != "GET" {
			return http.ErrUseLastResponse
		}
		if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
			req.Header.Del("JOB-TOKEN")
			req.Header.Del("PRIVATE-TOKEN")
			if req.URL.Scheme != "https" {
				return fmt.Errorf("insecure redirect")
			}
		}
		return nil
	}
	s.APIURL = strings.TrimRight(s.APIURL, "/")
	return &JobClient{settings: s, http: client}, nil
}

func (c *JobClient) request(
	ctx context.Context,
	method, path string,
	body io.Reader,
	size int64,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		c.settings.APIURL+"/projects/"+url.PathEscape(c.settings.ProjectID)+"/"+strings.TrimLeft(path, "/"),
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid API request")
	}
	req.Header.Set("JOB-TOKEN", c.settings.Token)
	req.Header.Set("Content-Type", "application/json")
	if size >= 0 {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("GitLab request failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, &HTTPError{Status: response.StatusCode}
	}
	return response, nil
}

func (c *JobClient) JSON(ctx context.Context, method, path string, input, output any) error {
	var reader io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	r, err := c.request(ctx, method, path, reader, -1)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		return err
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, (16<<20)+1))
	if err != nil {
		return fmt.Errorf("cannot read GitLab response")
	}
	if len(b) > 16<<20 {
		return fmt.Errorf("oversized GitLab JSON response")
	}
	if json.Unmarshal(b, output) != nil {
		return fmt.Errorf("invalid GitLab JSON response")
	}
	return nil
}

func (c *JobClient) Download(ctx context.Context, path, dest string) error {
	r, err := c.request(ctx, "GET", path, nil, -1)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r.Body)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(dest)
	}
	return err
}

func (c *JobClient) Upload(ctx context.Context, path, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	r, err := c.request(ctx, "PUT", path, f, info.Size())
	if err != nil {
		return err
	}
	return r.Body.Close()
}

func (c *JobClient) ProtectedTag(ctx context.Context, ref, sha string) error {
	if !operations.ReleaseTag.MatchString(ref) || !operations.FullSHA.MatchString(sha) {
		return fmt.Errorf("invalid production revision")
	}
	var tag struct {
		Protected bool `json:"protected"`
		Commit    struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.JSON(ctx, "GET", "repository/tags/"+url.PathEscape(ref), nil, &tag); err != nil {
		return err
	}
	if !tag.Protected || tag.Commit.ID != sha {
		return fmt.Errorf("production tag is unprotected or changed")
	}
	return nil
}

func (c *JobClient) ResolveRevision(ctx context.Context, ref string) (string, error) {
	if err := operations.ValidateRef(ref); err != nil {
		return "", err
	}
	var value struct {
		ID string `json:"id"`
	}
	err := c.JSON(ctx, "GET", "repository/commits/"+url.PathEscape(ref), nil, &value)
	if err != nil {
		return "", err
	}
	if !operations.FullSHA.MatchString(value.ID) {
		return "", fmt.Errorf("invalid resolved revision")
	}
	return value.ID, nil
}

func (c *JobClient) CreateDeployment(ctx context.Context, environment, sha, ref string) (int64, error) {
	if !operations.FullSHA.MatchString(sha) {
		return 0, fmt.Errorf("deployment requires full revision")
	}
	var deployment struct {
		ID int64 `json:"id"`
	}
	err := c.JSON(
		ctx,
		"POST",
		"deployments",
		map[string]any{
			"environment": "sistema/" + environment,
			"sha":         sha,
			"ref":         ref,
			"tag":         operations.ReleaseTag.MatchString(ref),
			"status":      "running",
		},
		&deployment,
	)
	if err != nil {
		return 0, err
	}
	if deployment.ID < 1 {
		return 0, fmt.Errorf("deployment creation response uncertain")
	}
	return deployment.ID, nil
}

func (c *JobClient) FinishDeployment(ctx context.Context, id int64, status string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = c.JSON(ctx, "PUT", fmt.Sprintf("deployments/%d", id), map[string]string{"status": status}, nil)
		if err == nil {
			return nil
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return err
}
