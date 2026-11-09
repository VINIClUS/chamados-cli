package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

func (e *execution) basic(ctx context.Context) error {
	if err := e.phase(e.env.Request.Operation); err != nil {
		return err
	}
	var output string
	var err error
	switch e.env.Request.Operation {
	case "status":
		output, err = e.composeOutput(ctx, "ps", "--all", "--format", "json")
	case "logs":
		output, err = e.composeOutput(
			ctx,
			append(
				[]string{"logs", "--no-color", "--tail", fmt.Sprint(e.env.Request.Tail)},
				e.env.Request.Services()...)...)
	case "restart":
		err = e.compose(
			ctx,
			nil,
			e.options.Diagnostics,
			append([]string{"restart"}, e.env.Request.Services()...)...)
		if err == nil {
			e.result.Applied = true
			err = e.health(ctx)
		}
	case "health":
		err = e.health(ctx)
	case "smoke":
		return e.smoke(ctx)
	default:
		return fmt.Errorf("unsupported basic operation")
	}
	e.setOutput(output)
	return err
}

func (e *execution) health(ctx context.Context) error {
	for _, target := range []string{e.options.BackendURL, e.options.FrontendURL} {
		if err := waitHTTP(ctx, target, e.options.HealthTimeout); err != nil {
			return err
		}
	}
	return nil
}

func waitHTTP(ctx context.Context, target string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
		if err != nil {
			return fmt.Errorf("invalid health URL")
		}
		response, err := client.Do(req)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check failed: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func (e *execution) smoke(ctx context.Context) error {
	settings, err := readEnv(filepath.Join(e.options.Dir, ".env"))
	if err != nil {
		return err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	base := strings.TrimSuffix(e.options.BackendURL, "/auth/csrf")
	call := func(method, path string, payload any, headers map[string]string) (map[string]any, error) {
		var body io.Reader
		if payload != nil {
			b, e := json.Marshal(payload)
			if e != nil {
				return nil, e
			}
			body = strings.NewReader(string(b))
		}
		req, e := http.NewRequestWithContext(ctx, method, base+path, body)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		r, e := client.Do(req)
		if e != nil {
			return nil, fmt.Errorf("smoke request failed")
		}
		defer r.Body.Close()
		if r.StatusCode < 200 || r.StatusCode >= 300 {
			return nil, fmt.Errorf("smoke HTTP %d", r.StatusCode)
		}
		result := map[string]any{}
		if method == "GET" {
			if e = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&result); e != nil {
				return nil, fmt.Errorf("invalid smoke response")
			}
		}
		return result, nil
	}
	csrf, err := call("GET", "/auth/csrf", nil, nil)
	if err != nil {
		return err
	}
	token, ok := csrf["token"].(string)
	if !ok || token == "" {
		return fmt.Errorf("missing CSRF token")
	}
	if _, err = call(
		"POST",
		"/auth/login",
		map[string]string{
			"email":    settings["APP_INITIALIZER_ADMIN_EMAIL"],
			"password": settings["APP_INITIALIZER_ADMIN_PASSWORD"],
		},
		map[string]string{"X-XSRF-TOKEN": token},
	); err != nil {
		return err
	}
	me, err := call("GET", "/auth/me", nil, nil)
	if err != nil {
		return err
	}
	secretaries, ok := me["secretaries"].([]any)
	if !ok || len(secretaries) == 0 {
		return fmt.Errorf("missing smoke secretary")
	}
	secretary, ok := secretaries[0].(map[string]any)
	if !ok || secretary["id"] == nil {
		return fmt.Errorf("invalid smoke secretary")
	}
	headers := map[string]string{"X-Active-Secretary-Id": fmt.Sprint(secretary["id"])}
	dashboard, err := call("GET", "/dashboard/calls", nil, headers)
	if err != nil {
		return err
	}
	if _, ok = dashboard["totalActive"].(float64); !ok {
		return fmt.Errorf("invalid dashboard total")
	}
	if _, ok = dashboard["urgent"].(float64); !ok {
		return fmt.Errorf("invalid urgent total")
	}
	if _, ok = dashboard["recentCalls"].([]any); !ok {
		return fmt.Errorf("invalid dashboard calls")
	}
	query := url.Values{
		"search":     {""},
		"status":     {"OPENED"},
		"priority":   {"NORMAL"},
		"unassigned": {"true"},
		"startDate":  {"2020-01-01"},
		"endDate":    {"2035-12-31"},
		"page":       {"0"},
		"size":       {"10"},
	}
	filtered, err := call("GET", "/calls/filter?"+query.Encode(), nil, headers)
	if err != nil {
		return err
	}
	if _, ok = filtered["content"].([]any); !ok {
		return fmt.Errorf("invalid filtered calls")
	}
	page, ok := filtered["page"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid filtered pagination")
	}
	if _, ok = page["number"].(float64); !ok {
		return fmt.Errorf("invalid page number")
	}
	if _, ok = page["totalElements"].(float64); !ok {
		return fmt.Errorf("invalid page total")
	}
	e.setOutput("Dashboard de chamados: OK\nConsulta combinada de chamados: OK\n")
	return nil
}
