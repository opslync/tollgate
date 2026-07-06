// Package k8s gives Tollgate in-cluster identity: it authenticates agent pods
// by their ServiceAccount token via the Kubernetes TokenReview API and enriches
// them with pod/workload metadata from a periodically-polled cache. It speaks
// to the API server with a minimal hand-rolled REST client — only three call
// shapes are needed, so client-go's weight isn't justified.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// Client is a minimal Kubernetes API-server REST client authenticated with the
// pod's own ServiceAccount token.
type Client struct {
	baseURL   string
	http      *http.Client
	tokenPath string // re-read per request: projected tokens rotate ~hourly
}

// NewClient builds an in-cluster client from the standard ServiceAccount mount
// and KUBERNETES_SERVICE_HOST/_PORT env vars. It errors when not running in a
// pod, so callers can surface a clear "kubernetes.enabled but not in-cluster".
func NewClient() (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in-cluster: KUBERNETES_SERVICE_HOST/_PORT unset")
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("cluster CA at %s is not valid PEM", caPath)
	}
	return &Client{
		baseURL: "https://" + net.JoinHostPort(host, port),
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
		tokenPath: tokenPath,
	}, nil
}

// doRequest issues one API call, JSON-encoding body (if any) and decoding a
// 2xx response into out (if any). Non-2xx responses become an error.
func (c *Client) doRequest(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if tok, err := c.token(); err != nil {
		return err
	} else if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("k8s API %s %s: %s: %s", method, path, resp.Status, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) token() (string, error) {
	if c.tokenPath == "" {
		return "", nil // tests point baseURL at a fake server with no auth
	}
	b, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("read service account token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
