package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

var ErrStore = errors.New("telemetry store unavailable")

const logStreamFields = "observatory.org,observatory.node,service.name"

type Client struct {
	MetricsURL, LogsURL string
	HTTP                *http.Client
}

func (c *Client) WriteMetrics(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) error {
	return c.post(ctx, c.MetricsURL+"/opentelemetry/v1/metrics", nil, req)
}

func (c *Client) WriteLogs(ctx context.Context, req *collogspb.ExportLogsServiceRequest) error {
	return c.post(ctx, c.LogsURL+"/insert/opentelemetry/v1/logs", map[string]string{"VL-Stream-Fields": logStreamFields}, req)
}

func (c *Client) post(ctx context.Context, url string, header map[string]string, msg proto.Message) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.do(c.httpClient(), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	return c.HTTP
}

// do maps transport errors and 5xx to ErrStore; on success the caller closes the body.
func (c *Client) do(hc *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: status %d", ErrStore, resp.StatusCode)
	}
	return nil, fmt.Errorf("telemetry store rejected request: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
}
