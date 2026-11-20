package telemetry

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

var testID = Identity{
	OrgID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	NodeID:   uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	NodeName: "node-107",
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func kvlist(k string, kvs ...*commonpb.KeyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}}
}

func attrMap(t *testing.T, attrs []*commonpb.KeyValue) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, kv := range attrs {
		m[kv.Key] = kv.Value.GetStringValue()
	}
	return m
}

func requireIdentity(t *testing.T, r *resourcepb.Resource) {
	t.Helper()
	m := attrMap(t, r.GetAttributes())
	if m["observatory.org"] != testID.OrgID.String() || m["observatory.node"] != testID.NodeID.String() || m["observatory.node_name"] != "node-107" {
		t.Fatalf("identity not stamped: %v", m)
	}
}

func evil() []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		str("observatory.org", "evil"),
		str("Observatory.Node", "evil"),
		str("observatory_org", "evil"),
		str("observatory-org", "evil"),
		kvlist("observatory", str("org", "evil")),
		str("orders", "7"),
	}
}

func requireOnlyClean(t *testing.T, where string, attrs []*commonpb.KeyValue) {
	t.Helper()
	if len(attrs) != 1 || attrs[0].Key != "orders" || attrs[0].Value.GetStringValue() != "7" {
		t.Fatalf("%s: reserved not stripped or clean attr lost: %v", where, attrs)
	}
}

func TestStampMetricsSetsIdentity(t *testing.T) {
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{
		{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "observatory-node")}}},
	}}
	StampMetrics(req, testID)
	r := req.ResourceMetrics[0].Resource
	requireIdentity(t, r)
	if attrMap(t, r.Attributes)["service.name"] != "observatory-node" {
		t.Fatal("service.name lost")
	}
}

func TestStampStripsReserved(t *testing.T) {
	ex := []*metricspb.Exemplar{{FilteredAttributes: evil()}}
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: append(evil(), str("status", "paid"))},
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope: &commonpb.InstrumentationScope{Attributes: evil()},
			Metrics: []*metricspb.Metric{
				{Metadata: evil(), Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{Attributes: evil(), Exemplars: ex}}}}},
				{Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{{Attributes: evil()}}}}},
				{Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{Attributes: evil(), Exemplars: ex}}}}},
				{Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: []*metricspb.ExponentialHistogramDataPoint{{Attributes: evil(), Exemplars: ex}}}}},
				{Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: []*metricspb.SummaryDataPoint{{Attributes: evil()}}}}},
			},
		}},
	}}}
	StampMetrics(req, testID)

	rm := req.ResourceMetrics[0]
	requireIdentity(t, rm.Resource)
	m := attrMap(t, rm.Resource.Attributes)
	if m["orders"] != "7" || m["status"] != "paid" {
		t.Fatalf("clean resource attrs lost: %v", m)
	}
	sm := rm.ScopeMetrics[0]
	requireOnlyClean(t, "scope", sm.Scope.Attributes)
	requireOnlyClean(t, "metadata", sm.Metrics[0].Metadata)
	g := sm.Metrics[0].GetGauge().DataPoints[0]
	requireOnlyClean(t, "gauge", g.Attributes)
	requireOnlyClean(t, "gauge exemplar", g.Exemplars[0].FilteredAttributes)
	requireOnlyClean(t, "sum", sm.Metrics[1].GetSum().DataPoints[0].Attributes)
	h := sm.Metrics[2].GetHistogram().DataPoints[0]
	requireOnlyClean(t, "histogram", h.Attributes)
	requireOnlyClean(t, "histogram exemplar", h.Exemplars[0].FilteredAttributes)
	eh := sm.Metrics[3].GetExponentialHistogram().DataPoints[0]
	requireOnlyClean(t, "exp histogram", eh.Attributes)
	requireOnlyClean(t, "exp histogram exemplar", eh.Exemplars[0].FilteredAttributes)
	requireOnlyClean(t, "summary", sm.Metrics[4].GetSummary().DataPoints[0].Attributes)
}

func TestStampLogsSetsIdentity(t *testing.T) {
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: append(evil(), str("service.name", "observatory-node"))},
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Attributes: evil()},
			LogRecords: []*logspb.LogRecord{{Attributes: append(evil(), str("vm", "vm01"))}},
		}},
	}}}
	StampLogs(req, testID)

	rl := req.ResourceLogs[0]
	requireIdentity(t, rl.Resource)
	requireOnlyClean(t, "scope", rl.ScopeLogs[0].Scope.Attributes)
	m := attrMap(t, rl.ScopeLogs[0].LogRecords[0].Attributes)
	if len(m) != 2 || m["vm"] != "vm01" || m["orders"] != "7" {
		t.Fatalf("log record attrs: %v", m)
	}
	for k := range attrMap(t, rl.Resource.Attributes) {
		if strings.HasPrefix(strings.ToLower(k), "observatory") && !strings.HasPrefix(k, ReservedPrefix) {
			t.Fatalf("lookalike key survived: %s", k)
		}
	}
}

func TestStampNilResource(t *testing.T) {
	m := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{}}}
	StampMetrics(m, testID)
	requireIdentity(t, m.ResourceMetrics[0].Resource)
	l := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{nil, {}}}
	StampLogs(l, testID)
	requireIdentity(t, l.ResourceLogs[1].Resource)
}
