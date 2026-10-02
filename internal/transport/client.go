package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"netlab.local/core/api"
	"os"
	"time"
)

type Client struct{ HTTP *http.Client }

func NewClient(caFile, certFile, keyFile string) (*Client, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid node CA")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &Client{HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}}, MaxIdleConns: 256, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 20 * time.Minute}}}, nil
}
func (c *Client) Do(ctx context.Context, method, endpoint, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("node %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16384))
		if readErr != nil {
			return readErr
		}
		return fmt.Errorf("node %s returned %d: %s", endpoint, resp.StatusCode, raw)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(output)
}
func (c *Client) Info(ctx context.Context, endpoint string) (api.NodeInfo, error) {
	var result api.NodeInfo
	err := c.Do(ctx, http.MethodGet, endpoint, "/node/v1/info", nil, &result)
	return result, err
}
func (c *Client) Execute(ctx context.Context, endpoint string, plan api.NodePlan) (api.NodeResult, error) {
	var result api.NodeResult
	err := c.Do(ctx, http.MethodPost, endpoint, "/node/v1/plans", plan, &result)
	return result, err
}
