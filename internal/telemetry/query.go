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
	"cpu":  {`sum by (state) (rate(system_cpu_time_seconds_total{observatory_node="$node",observatory_vm="",state=~"user|system|iowait"}[$step])) / scalar(count(count by (cpu) (system_cpu_time_seconds_total{observatory_node="$node",observatory_vm=""}))) * 100`, []string{"state"}},
	"mem":  {`sum by (state) (system_memory_usage_bytes{observatory_node="$node",observatory_vm="",state=~"used|cached"})`, []string{"state"}},
	"disk": {`100 * system_filesystem_usage_bytes{observatory_node="$node",observatory_vm=""} / system_filesystem_limit_bytes{observatory_node="$node",observatory_vm=""}`, []string{"mountpoint"}},
	"net":  {`sum by (device, direction) (rate(system_network_io_bytes_total{observatory_node="$node",observatory_vm=""}[$step]))`, []string{"device", "direction"}},
}

func usageStep(from, to time.Time) time.Duration {
	return max(to.Sub(from)/300, 10*time.Second).Truncate(time.Second)
}

const nodeScope = `observatory_node="$node",observatory_vm=""`

func (c *Client) NodeUsage(ctx context.Context, org, node uuid.UUID, from, to time.Time) (Usage, error) {
	return c.usage(ctx, org, "observatory_node", node, from, to)
}

func (c *Client) VMUsage(ctx context.Context, org, vm uuid.UUID, from, to time.Time) (Usage, error) {
	return c.usage(ctx, org, "observatory_vm", vm, from, to)
}

func (c *Client) usage(ctx context.Context, org uuid.UUID, label string, id uuid.UUID, from, to time.Time) (Usage, error) {
	step := fmt.Sprintf("%ds", int64(usageStep(from, to).Seconds()))
	expand := strings.NewReplacer("$node", id.String(), "$step", step)
	scope := nodeScope
	if label != "observatory_node" {
		scope = label + `="$node"`
	}
	points := map[string][]map[int64]float64{}
	names := map[string][]string{}
	stamps := map[int64]bool{}
	for chart, nq := range nodeQueries {
		form := url.Values{
			"query":       {expand.Replace(strings.ReplaceAll(nq.expr, nodeScope, scope))},
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
	Org, Node, VM       uuid.UUID
	From, To            time.Time
	Text, Level, Source string
	Stream              string
	Fields              map[string]string
	Limit               int
}

type LogLine struct {
	Time   time.Time      `json:"time"`
	Level  string         `json:"level"`
	Source string         `json:"source"`
	Stream string         `json:"stream,omitempty"`
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields"`
}

// severityText maps API levels to the OTLP severity text written by the slog bridge; unknown levels add no filter.
var severityText = map[string]string{"debug": "DEBUG", "info": "INFO", "warn": "WARN", "error": "ERROR"}

// buildLogsQL puts user input only into quoted string literals (field names included, since bare names can be keywords like "not").
func buildLogsQL(q LogQuery) string {
	var parts []string
	if !q.From.IsZero() && !q.To.IsZero() {
		parts = append(parts, "_time:["+q.From.UTC().Format(time.RFC3339Nano)+", "+q.To.UTC().Format(time.RFC3339Nano)+"]")
	}
	if q.Text != "" {
		parts = append(parts, "*:"+strconv.Quote(q.Text))
	}
	if sev, ok := severityText[q.Level]; ok {
		parts = append(parts, `"severity_text":=`+strconv.Quote(sev))
	}
	if q.Stream != "" {
		parts = append(parts, `"observatory.stream":=`+strconv.Quote(q.Stream))
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
	if (q.Node == uuid.Nil) == (q.VM == uuid.Nil) {
		return errors.New("telemetry: log query needs exactly one of node or vm")
	}
	scope := " observatory.node:=" + strconv.Quote(q.Node.String()) + ` observatory.vm:=""`
	if q.VM != uuid.Nil {
		scope = " observatory.vm:=" + strconv.Quote(q.VM.String())
	}
	form.Set("query", buildLogsQL(q))
	form.Set("extra_filters", "observatory.org:="+strconv.Quote(q.Org.String())+scope)
	resp, err := c.postForm(ctx, hc, c.LogsURL+path, form)
	if resp != nil {
		defer resp.Body.Close()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
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
	l := LogLine{Time: t, Level: strings.ToLower(rec["severity_text"]), Source: rec["source"], Stream: rec["observatory.stream"], Msg: rec["_msg"], Fields: map[string]any{}}
	for k, v := range rec {
		switch k {
		case "_time", "_msg", "_stream", "_stream_id", "severity_text", "source", "observatory.stream":
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

type VMNow struct {
	At                                                         time.Time
	CPUPercent                                                 float64
	MemUsedBytes, MemTotalBytes, DiskUsedBytes, DiskTotalBytes int64
}

type instantSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

func (c *Client) instant(ctx context.Context, org uuid.UUID, query string, at time.Time) ([]instantSample, error) {
	form := url.Values{
		"query":       {query},
		"time":        {strconv.FormatInt(at.Unix(), 10)},
		"extra_label": {"observatory_org=" + org.String()},
	}
	resp, err := c.postForm(ctx, c.httpClient(), c.MetricsURL+"/api/v1/query", form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Result []instantSample `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode query: %v", ErrStore, err)
	}
	return body.Data.Result, nil
}

func (s instantSample) number() (float64, time.Time, bool) {
	ts, _ := s.Value[0].(float64)
	str, _ := s.Value[1].(string)
	v, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, time.Time{}, false
	}
	return v, time.Unix(int64(ts), 0), true
}

// VMLatest returns the newest values per VM; VMs without data are absent.
func (c *Client) VMLatest(ctx context.Context, org uuid.UUID, vms []uuid.UUID) (map[uuid.UUID]VMNow, error) {
	out := map[uuid.UUID]VMNow{}
	if len(vms) == 0 {
		return out, nil
	}
	ids := make([]string, len(vms))
	for i, vm := range vms {
		ids[i] = vm.String()
	}
	sel := `observatory_vm=~"` + strings.Join(ids, "|") + `"`
	cpu := `system_cpu_time_seconds_total{` + sel + `}`
	metrics := []struct {
		query string
		set   func(*VMNow, float64)
	}{
		{`100 * sum by (observatory_vm) (rate(system_cpu_time_seconds_total{` + sel + `,state=~"user|system|iowait"}[1m])) / count by (observatory_vm) (count by (observatory_vm, cpu) (` + cpu + `))`, func(n *VMNow, v float64) { n.CPUPercent = v }},
		{`sum by (observatory_vm) (system_memory_usage_bytes{` + sel + `,state="used"})`, func(n *VMNow, v float64) { n.MemUsedBytes = int64(v) }},
		{`sum by (observatory_vm) (system_memory_usage_bytes{` + sel + `,state=~"used|cached|free"})`, func(n *VMNow, v float64) { n.MemTotalBytes = int64(v) }},
		{`sum by (observatory_vm) (system_filesystem_usage_bytes{` + sel + `,mountpoint="/"})`, func(n *VMNow, v float64) { n.DiskUsedBytes = int64(v) }},
		{`sum by (observatory_vm) (system_filesystem_limit_bytes{` + sel + `,mountpoint="/"})`, func(n *VMNow, v float64) { n.DiskTotalBytes = int64(v) }},
	}
	now := time.Now()
	for _, m := range metrics {
		res, err := c.instant(ctx, org, m.query, now)
		if err != nil {
			return nil, err
		}
		for _, r := range res {
			vm, err := uuid.Parse(r.Metric["observatory_vm"])
			v, _, ok := r.number()
			if err != nil || !ok {
				continue
			}
			n := out[vm]
			m.set(&n, v)
			out[vm] = n
		}
	}
	stamps, err := c.instant(ctx, org, `max by (observatory_vm) (timestamp(system_memory_usage_bytes{`+sel+`,state="used"}))`, now)
	if err != nil {
		return nil, err
	}
	for _, r := range stamps {
		vm, perr := uuid.Parse(r.Metric["observatory_vm"])
		v, _, ok := r.number()
		if n, found := out[vm]; perr == nil && ok && found {
			n.At = time.Unix(int64(v), 0)
			out[vm] = n
		}
	}
	return out, nil
}

// VMDrops returns telemetry records the node dropped for the VM in [from, to], by kind.
func (c *Client) VMDrops(ctx context.Context, org, vm uuid.UUID, from, to time.Time) (map[string]int64, error) {
	window := max(int64(to.Sub(from).Seconds()), 1)
	// increase_pure counts a counter that appears inside the window from 0: the node exports it only every 10 s, after drops may have begun.
	q := fmt.Sprintf(`sum by (kind) (increase_pure(observatory_vm_telemetry_dropped_total{vm=%s,observatory_vm="",observatory_node!=""}[%ds]))`, strconv.Quote(vm.String()), window)
	res, err := c.instant(ctx, org, q, to)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, r := range res {
		if v, _, ok := r.number(); ok {
			out[r.Metric["kind"]] = int64(math.Round(v))
		}
	}
	return out, nil
}
