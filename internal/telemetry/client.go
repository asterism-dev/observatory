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
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: status %d", ErrStore, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry store rejected write: status %d", resp.StatusCode)
	}
	return nil
}
