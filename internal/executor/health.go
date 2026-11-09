package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Public checks retain the requested hostname for HTTP Host and TLS SNI while
// allowing the LAN runner to reach the edge without hairpin NAT.
func checkPublic(ctx context.Context, environment string) error {
	base := os.Getenv("DEPLOY_URL")
	if base == "" {
		base = "https://stg-chamados.portosoftware.com.br"
		if environment == "production" {
			base = "https://chamados.portosoftware.com.br"
		}
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return fmt.Errorf("invalid public health URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	if edge := os.Getenv("DEPLOY_EDGE_IP"); edge != "" {
		if net.ParseIP(edge) == nil {
			return fmt.Errorf("invalid edge IP")
		}
		transport.Proxy = nil
		transport.DialContext = func(callCtx context.Context, network, address string) (net.Conn, error) {
			host, port, e := net.SplitHostPort(address)
			if e != nil {
				return nil, e
			}
			if host == u.Hostname() {
				address = net.JoinHostPort(edge, port)
			}
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(callCtx, network, address)
		}
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, path := range []string{"/", "/api/auth/csrf"} {
		if err = waitPublic(ctx, client, strings.TrimRight(base, "/")+path); err != nil {
			return err
		}
	}
	return nil
}

func waitPublic(parent context.Context, client *http.Client, target string) error {
	ctx, cancel := context.WithTimeout(parent, 240*time.Second)
	defer cancel()
	for {
		request, err := http.NewRequestWithContext(ctx, "GET", target, nil)
		if err != nil {
			return fmt.Errorf("invalid public health request")
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("public health failed: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
