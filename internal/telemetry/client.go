package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
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

const (
	NodeLogStreamFields  = "observatory.org,observatory.node,service.name"
	VMLogStreamFields    = "observatory.org,observatory.vm,observatory.stream"
	BuildLogStreamFields = "observatory.org,observatory.image,observatory.stream"
)

type Client struct {
	MetricsURL, LogsURL string
	HTTP                *http.Client
}

func (c *Client) WriteMetrics(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) error {
	return c.post(ctx, c.MetricsURL+"/opentelemetry/v1/metrics", nil, req)
}

func (c *Client) WriteLogs(ctx context.Context, req *collogspb.ExportLogsServiceRequest, streamFields string) error {
	return c.post(ctx, c.LogsURL+"/insert/opentelemetry/v1/logs", map[string]string{"VL-Stream-Fields": streamFields}, req)
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

func (c *Client) httpClient() *http.Client { return c.StoreClient(10 * time.Second) }

// StoreClient is c.HTTP (or a default client) with timeout that never follows redirects: a followed POST becomes a GET without the scope params.
func (c *Client) StoreClient(timeout time.Duration) *http.Client {
	var hc http.Client
	if c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.Timeout = timeout
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
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
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	// A redirect is never followed, so it is as unusable as a 5xx.
	if resp.StatusCode >= 500 || resp.StatusCode < 400 {
		return nil, fmt.Errorf("%w: status %d", ErrStore, resp.StatusCode)
	}
	rej := &RejectedError{Status: resp.StatusCode, Msg: string(bytes.TrimSpace(msg))}
	var body struct{ Error string }
	if json.Unmarshal(msg, &body) == nil && body.Error != "" {
		rej.Msg = body.Error
	}
	return nil, rej
}

// RejectedError is a 4xx answer of the store, such as a query that does not parse; Msg is the store's error text.
type RejectedError struct {
	Status int
	Msg    string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("telemetry store rejected request: status %d: %s", e.Status, e.Msg)
}
