package telemetry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/asterism-dev/observatory/internal/telemetry"
)

var logsEndpoints = []string{"query", "tail", "hits", "field_names", "field_values", "stream_field_names", "stream_field_values", "streams", "stream_ids", "stats_query", "stats_query_range", "facets"}

// Each case runs unscoped first to prove it reaches org B, then under org A's forced extra_filters.
func TestVictoriaLogsScopesUserQueries(t *testing.T) {
	logsURL := os.Getenv("OBSERVATORY_TEST_LOGS_URL")
	if logsURL == "" {
		t.Skip("OBSERVATORY_TEST_LOGS_URL not set")
	}
	run := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	orgA, orgB := uuid.New(), uuid.New()
	mA, mB, shared := "vlprobea"+run, "vlprobeb"+run, "vlprobesh"+run
	client := &telemetry.Client{LogsURL: logsURL}
	writeProbeLogs(t, client, orgA, mA, run, shared, "", telemetry.NodeLogStreamFields)
	// Org B gets a private field (also a stream field) so field and stream-field names can reveal it.
	writeProbeLogs(t, client, orgB, mB, run, shared, mB, telemetry.NodeLogStreamFields+","+mB)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(5*time.Minute)
	call := func(ep, q, field string, scope uuid.UUID) (int, string) {
		form := url.Values{"query": {q}}
		switch ep {
		case "tail":
			form.Set("start_offset", "1h")
		case "stats_query":
			form.Set("query", q+" | stats by (observatory.org, "+mB+") count() c")
			form.Set("time", to.Format(time.RFC3339))
		case "stats_query_range":
			form.Set("query", q+" | stats by (observatory.org, "+mB+") count() c")
			form.Set("step", "1h")
		case "hits":
			form.Set("step", "1h")
			form["field"] = []string{"observatory.org", mB}
		case "field_values":
			form.Set("field", field)
		case "stream_field_values":
			form.Set("field", "observatory.org")
		case "facets":
			form.Set("keep_const_fields", "1")
		}
		if ep != "tail" && ep != "stats_query" {
			form.Set("start", from.Format(time.RFC3339))
			form.Set("end", to.Format(time.RFC3339))
		}
		if scope != uuid.Nil {
			form.Set("extra_filters", fmt.Sprintf("observatory.org:=%q", scope.String()))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, logsURL+"/select/logsql/"+ep, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		// tail sends no headers until it has a row, so a silent tail times out here.
		if ep == "tail" && errors.Is(err, context.DeadlineExceeded) {
			return http.StatusOK, ""
		}
		if err != nil {
			t.Fatalf("%s: %v", ep, err)
		}
		defer resp.Body.Close()
		// tail streams until the deadline; what arrived so far is the answer.
		body, err := io.ReadAll(resp.Body)
		if err != nil && ep != "tail" {
			t.Fatalf("%s: %v", ep, err)
		}
		return resp.StatusCode, string(body)
	}

	waitFor(t, "probe logs", func() bool {
		_, body := call("query", "probe:="+run, "", uuid.Nil)
		return strings.Count(body, "\n") == 6
	})
	streamID := func(org uuid.UUID) string {
		_, body := call("stream_ids", fmt.Sprintf("observatory.org:=%q", org.String()), "", uuid.Nil)
		var out struct{ Values []struct{ Value string } }
		if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Values) != 1 {
			t.Fatalf("stream id of %s: %v %s", org, err, body)
		}
		return out.Values[0].Value
	}
	sidA, sidB := streamID(orgA), streamID(orgB)

	// oracle cases return org A rows only when a subquery saw org B's values.
	cases := []struct {
		construct, query string
		oracle           bool
	}{
		{"word filter", mB, false},
		{"org field filter", fmt.Sprintf("observatory.org:=%q", orgB.String()), false},
		{"_stream filter", fmt.Sprintf("_stream:{observatory.org=%q}", orgB.String()), false},
		{"_stream_id filter", "_stream_id:" + sidB, false},
		{"or", mA + " or " + mB, false},
		{"options(ignore_global_time_filter)", "options(ignore_global_time_filter=true) " + mB, false},
		{"options(global_filter)", fmt.Sprintf("options(global_filter=(observatory.org:=%q)) %s", orgB.String(), mB), false},
		{"in", fmt.Sprintf("%s k:in(%s | fields k)", mA, mB), true},
		{"in with options", fmt.Sprintf("%s k:in(options(ignore_global_time_filter=true) %s | fields k)", mA, mB), true},
		{"contains_any", fmt.Sprintf("%s _msg:contains_any(%s | fields k)", mA, mB), true},
		// contains_all of an empty set is true, so the oracle negates a value org A lacks.
		{"contains_all", fmt.Sprintf("%s -_msg:contains_all(%s | fields %s)", mA, mB, mB), true},
		{"_stream_id:in", fmt.Sprintf("_stream_id:in(%s | fields _stream_id)", mB), false},
		{"filter pipe with in", fmt.Sprintf("%s | filter k:in(%s | fields k)", mA, mB), true},
		{"stats if with in", fmt.Sprintf("%s | stats by (observatory.org) count() if (k:in(%s | fields k)) c | filter c:>0", mA, mB), true},
		{"union", fmt.Sprintf("%s | union (%s)", mA, mB), false},
		{"union with options", fmt.Sprintf("%s | union (options(ignore_global_time_filter=true) %s)", mA, mB), false},
		{"union with stream_context", fmt.Sprintf("%s | union (%s | stream_context before 10 after 10)", mA, mB), false},
		{"join", fmt.Sprintf("%s | join by (probe) (%s | fields probe, observatory.org, %s)", mA, mB, mB), false},
		{"join inner", fmt.Sprintf("%s | join by (probe) (%s | fields probe, %s) inner", mA, mB, mB), false},
		{"stream_context", "probe:=" + run + " | stream_context before 10 after 10", false},
		{"facets pipe", "probe:=" + run + " | facets", false},
		{"stats pipe", "probe:=" + run + " | stats by (observatory.org) count() c", false},
		{"field_values pipe", "probe:=" + run + " | field_values observatory.org", false},
		{"field_names pipe", "probe:=" + run + " | field_names", false},
		{"uniq pipe", "probe:=" + run + " | uniq by (observatory.org)", false},
		{"top pipe", "probe:=" + run + " | top 10 by (observatory.org)", false},
	}
	seesB := func(body string) bool {
		return strings.Contains(body, `":"`+mB) || strings.Contains(body, orgB.String()) || strings.Contains(body, sidB)
	}
	seesA := func(body string) bool {
		for _, s := range []string{mA, orgA.String(), sidA, `"probe"`, `"observatory.org"`} {
			if strings.Contains(body, s) {
				return true
			}
		}
		return false
	}
	for _, c := range cases {
		for _, ep := range logsEndpoints {
			field, leaks := mB, seesB
			if c.oracle {
				field, leaks = "_msg", seesA
			}
			status, body := call(ep, c.query, field, uuid.Nil)
			if status != http.StatusOK {
				t.Logf("PROBE | %s | %s | unsupported (%d) | %q", ep, c.construct, status, c.query)
				continue
			}
			if c.oracle {
				// An endpoint that drops the construct returns org A rows even when the subquery matches nothing.
				_, blind := call(ep, strings.ReplaceAll(c.query, mB, "vlprobenone"+run), field, uuid.Nil)
				if seesA(blind) {
					body = ""
				}
			}
			if !leaks(body) {
				t.Logf("PROBE | %s | %s | no signal: unscoped result shows no org B data | %q | %.200s", ep, c.construct, c.query, body)
				if ep == "query" {
					t.Errorf("%s: unscoped query does not reach org B, probe proves nothing: %q -> %.300s", c.construct, c.query, body)
				}
				continue
			}
			status, body = call(ep, c.query, field, orgA)
			leaked := status == http.StatusOK && leaks(body)
			t.Logf("PROBE | %s | %s | leaks=%v (%d) | %q | %.200s", ep, c.construct, leaked, status, c.query, body)
			if leaked != slices.Contains(telemetry.LeakingLogsQL, c.construct) {
				t.Errorf("%s %s: leaked=%v disagrees with LeakingLogsQL: %q -> %.300s", ep, c.construct, leaked, c.query, body)
			}
		}
	}
}

func writeProbeLogs(t *testing.T, client *telemetry.Client, org uuid.UUID, marker, run, shared, privateKey, streamFields string) {
	t.Helper()
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	attrs := []*commonpb.KeyValue{str("probe", run), str("k", shared)}
	if privateKey != "" {
		attrs = append(attrs, str(privateKey, marker))
	}
	var records []*logspb.LogRecord
	for i := range 3 {
		records = append(records, &logspb.LogRecord{
			TimeUnixNano: uint64(time.Now().Add(time.Duration(i) * time.Millisecond).UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprintf("%s %s line %d", marker, shared, i)}},
			Attributes:   attrs,
		})
	}
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "vlprobe")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
	}}}
	telemetry.StampLogs(req, telemetry.Identity{OrgID: org, NodeID: uuid.New(), NodeName: "vlprobe"})
	if err := client.WriteLogs(context.Background(), req, streamFields); err != nil {
		t.Fatal(err)
	}
}
