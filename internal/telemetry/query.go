package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Usage struct {
	T      []int64             `json:"t"`
	Charts map[string][]Series `json:"charts"`
}

type Series struct {
	Name   string     `json:"name"`
	Values []*float64 `json:"values"`
}

type nodeQuery struct {
	expr string
	by   []string
}

// $node and $step are substituted per request; label names follow VictoriaMetrics' Prometheus naming of OTel metrics.
var nodeQueries = map[string]nodeQuery{
	"cpu":  {`sum by (state) (rate(system_cpu_time_seconds_total{observatory_node="$node",state=~"user|system|iowait"}[$step])) / scalar(count(count by (cpu) (system_cpu_time_seconds_total{observatory_node="$node"}))) * 100`, []string{"state"}},
	"mem":  {`sum by (state) (system_memory_usage_bytes{observatory_node="$node",state=~"used|cached"})`, []string{"state"}},
	"disk": {`100 * system_filesystem_usage_bytes{observatory_node="$node"} / system_filesystem_limit_bytes{observatory_node="$node"}`, []string{"mountpoint"}},
	"net":  {`sum by (device, direction) (rate(system_network_io_bytes_total{observatory_node="$node"}[$step]))`, []string{"device", "direction"}},
}

func usageStep(from, to time.Time) time.Duration {
	return max(to.Sub(from)/300, 10*time.Second).Truncate(time.Second)
}

func (c *Client) NodeUsage(ctx context.Context, org, node uuid.UUID, from, to time.Time) (Usage, error) {
	step := fmt.Sprintf("%ds", int64(usageStep(from, to).Seconds()))
	expand := strings.NewReplacer("$node", node.String(), "$step", step)
	points := map[string][]map[int64]float64{}
	names := map[string][]string{}
	stamps := map[int64]bool{}
	for chart, nq := range nodeQueries {
		form := url.Values{
			"query":       {expand.Replace(nq.expr)},
			"start":       {strconv.FormatInt(from.Unix(), 10)},
			"end":         {strconv.FormatInt(to.Unix(), 10)},
			"step":        {step},
			"extra_label": {"observatory_org=" + org.String()},
		}
		resp, err := c.postForm(ctx, c.httpClient(), c.MetricsURL+"/api/v1/query_range", form)
		if err != nil {
			return Usage{}, err
		}
		var body struct {
			Data struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
					Values [][2]any          `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return Usage{}, fmt.Errorf("%w: decode query_range: %v", ErrStore, err)
		}
		points[chart] = []map[int64]float64{}
		for _, r := range body.Data.Result {
			var label []string
			for _, l := range nq.by {
				label = append(label, r.Metric[l])
			}
			names[chart] = append(names[chart], strings.Join(label, " "))
			vals := map[int64]float64{}
			for _, p := range r.Values {
				ts, _ := p[0].(float64)
				s, _ := p[1].(string)
				v, err := strconv.ParseFloat(s, 64)
				// NaN/Inf are gaps: they can't be encoded as JSON.
				if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
					continue
				}
				vals[int64(ts)] = v
				stamps[int64(ts)] = true
			}
			points[chart] = append(points[chart], vals)
		}
	}
	u := Usage{T: []int64{}, Charts: map[string][]Series{}}
	for ts := range stamps {
		u.T = append(u.T, ts)
	}
	slices.Sort(u.T)
	for chart, series := range points {
		u.Charts[chart] = []Series{}
		for i, vals := range series {
			s := Series{Name: names[chart][i], Values: make([]*float64, len(u.T))}
			for j, ts := range u.T {
				if v, ok := vals[ts]; ok {
					s.Values[j] = &v
				}
			}
			u.Charts[chart] = append(u.Charts[chart], s)
		}
	}
	return u, nil
}

type LogQuery struct {
	Org, Node           uuid.UUID
	From, To            time.Time
	Text, Level, Source string
	Fields              map[string]string
	Limit               int
}

type LogLine struct {
	Time   time.Time      `json:"time"`
	Level  string         `json:"level"`
	Source string         `json:"source"`
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields"`
}

// buildLogsQL puts user input only into quoted string literals (field names included, since bare names can be keywords like "not").
func buildLogsQL(q LogQuery) string {
	var parts []string
	if !q.From.IsZero() && !q.To.IsZero() {
		parts = append(parts, "_time:["+q.From.UTC().Format(time.RFC3339Nano)+", "+q.To.UTC().Format(time.RFC3339Nano)+"]")
	}
	if q.Text != "" {
		parts = append(parts, "*:"+strconv.Quote(q.Text))
	}
	if q.Level != "" {
		parts = append(parts, "severity_text:="+strconv.Quote(q.Level))
	}
	if q.Source != "" {
		parts = append(parts, "source:="+strconv.Quote(q.Source))
	}
	keys := make([]string, 0, len(q.Fields))
	for k := range q.Fields {
		if k != "" && !strings.HasPrefix(k, ReservedPrefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		parts = append(parts, strconv.Quote(k)+":="+strconv.Quote(q.Fields[k]))
	}
	if len(parts) == 0 {
		return "*"
	}
	return strings.Join(parts, " ")
}

func (c *Client) Logs(ctx context.Context, q LogQuery) ([]LogLine, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	lines := []LogLine{}
	err := c.streamLogs(ctx, c.httpClient(), "/select/logsql/query", q, url.Values{"limit": {strconv.Itoa(limit)}}, func(l LogLine) error {
		lines = append(lines, l)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.After(lines[j].Time) })
	return lines, nil
}

// TailLogs streams new lines until ctx ends or emit fails; q.From/q.To are ignored.
func (c *Client) TailLogs(ctx context.Context, q LogQuery, emit func(LogLine) error) error {
	hc := *c.httpClient()
	hc.Timeout = 0
	q.From, q.To = time.Time{}, time.Time{}
	return c.streamLogs(ctx, &hc, "/select/logsql/tail", q, url.Values{}, emit)
}

func (c *Client) streamLogs(ctx context.Context, hc *http.Client, path string, q LogQuery, form url.Values, emit func(LogLine) error) error {
	form.Set("query", buildLogsQL(q))
	form.Set("extra_filters", "observatory.org:="+strconv.Quote(q.Org.String())+" observatory.node:="+strconv.Quote(q.Node.String()))
	resp, err := c.postForm(ctx, hc, c.LogsURL+path, form)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var rec map[string]string
		if err := dec.Decode(&rec); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: decode logs: %v", ErrStore, err)
		}
		if err := emit(toLogLine(rec)); err != nil {
			return err
		}
	}
}

func toLogLine(rec map[string]string) LogLine {
	t, _ := time.Parse(time.RFC3339Nano, rec["_time"])
	l := LogLine{Time: t, Level: rec["severity_text"], Source: rec["source"], Msg: rec["_msg"], Fields: map[string]any{}}
	for k, v := range rec {
		switch k {
		case "_time", "_msg", "_stream", "_stream_id", "severity_text", "source":
		default:
			l.Fields[k] = v
		}
	}
	return l
}

func (c *Client) postForm(ctx context.Context, hc *http.Client, target string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(hc, req)
}
