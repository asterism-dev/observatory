package telemetry

import (
	"strings"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

const ReservedPrefix = "observatory."

type Identity struct {
	OrgID, NodeID uuid.UUID
	NodeName      string
}

func StampMetrics(req *colmetricspb.ExportMetricsServiceRequest, id Identity) {
	for _, rm := range req.GetResourceMetrics() {
		if rm == nil {
			continue
		}
		rm.Resource = stampResource(rm.Resource, id)
		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				continue
			}
			stripScope(sm.Scope)
			for _, m := range sm.Metrics {
				if m != nil {
					stripMetric(m)
				}
			}
		}
	}
}

func StampLogs(req *collogspb.ExportLogsServiceRequest, id Identity) {
	for _, rl := range req.GetResourceLogs() {
		if rl == nil {
			continue
		}
		rl.Resource = stampResource(rl.Resource, id)
		for _, sl := range rl.ScopeLogs {
			if sl == nil {
				continue
			}
			stripScope(sl.Scope)
			for _, lr := range sl.LogRecords {
				if lr != nil {
					lr.Attributes = filterReserved(lr.Attributes)
				}
			}
		}
	}
}

func stampResource(r *resourcepb.Resource, id Identity) *resourcepb.Resource {
	if r == nil {
		r = &resourcepb.Resource{}
	}
	r.EntityRefs = nil
	r.Attributes = append(filterReserved(r.Attributes),
		stringAttr(ReservedPrefix+"org", id.OrgID.String()),
		stringAttr(ReservedPrefix+"node", id.NodeID.String()),
		stringAttr(ReservedPrefix+"node_name", id.NodeName),
	)
	return r
}

func stripScope(s *commonpb.InstrumentationScope) {
	if s != nil {
		s.Attributes = filterReserved(s.Attributes)
	}
}

func stripMetric(m *metricspb.Metric) {
	m.Metadata = filterReserved(m.Metadata)
	switch d := m.Data.(type) {
	case *metricspb.Metric_Gauge:
		stripNumberPoints(d.Gauge.GetDataPoints())
	case *metricspb.Metric_Sum:
		stripNumberPoints(d.Sum.GetDataPoints())
	case *metricspb.Metric_Histogram:
		for _, p := range d.Histogram.GetDataPoints() {
			p.Attributes = filterReserved(p.Attributes)
			stripExemplars(p.Exemplars)
		}
	case *metricspb.Metric_ExponentialHistogram:
		for _, p := range d.ExponentialHistogram.GetDataPoints() {
			p.Attributes = filterReserved(p.Attributes)
			stripExemplars(p.Exemplars)
		}
	case *metricspb.Metric_Summary:
		for _, p := range d.Summary.GetDataPoints() {
			p.Attributes = filterReserved(p.Attributes)
		}
	}
}

func stripNumberPoints(points []*metricspb.NumberDataPoint) {
	for _, p := range points {
		p.Attributes = filterReserved(p.Attributes)
		stripExemplars(p.Exemplars)
	}
}

func stripExemplars(exemplars []*metricspb.Exemplar) {
	for _, e := range exemplars {
		e.FilteredAttributes = filterReserved(e.FilteredAttributes)
	}
}

func filterReserved(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	kept := attrs[:0]
	for _, kv := range attrs {
		if kv != nil && !isReserved(kv.Key) {
			kept = append(kept, kv)
		}
	}
	return kept
}

// The stores flatten dots and other separators to "_" (metric labels) or nest kvlists into dotted fields (logs), so lookalike keys must go too.
func isReserved(key string) bool {
	norm := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, key)
	return norm == "observatory" || strings.HasPrefix(norm, "observatory_")
}

func stringAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}
