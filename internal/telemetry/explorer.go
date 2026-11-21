package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type GroupKind string

const (
	KindVM    GroupKind = "vm"
	KindNode  GroupKind = "node"
	KindImage GroupKind = "image"
)

// MaxExplorerRows caps the rows of an ExplorerQuery.
const MaxExplorerRows = 1000

type groupFields struct{ key, name, stream, extra string }

// Node streams are keyed by the slog source attribute: service.name is "observatory-node" for firecracker lines too.
var groupKinds = map[GroupKind]groupFields{
	KindVM:    {key: "observatory.vm", name: "observatory.vm_name", stream: "observatory.stream"},
	KindNode:  {key: "observatory.node", name: "observatory.node_name", stream: "source", extra: `observatory.vm:=""`},
	KindImage: {key: "observatory.image", name: "observatory.image_name", stream: "observatory.build"},
}

var groupOrder = []GroupKind{KindVM, KindNode, KindImage}

func ParseGroupKind(s string) (GroupKind, bool) {
	_, ok := groupKinds[GroupKind(s)]
	return GroupKind(s), ok
}

type GroupRef struct {
	Kind GroupKind
	ID   uuid.UUID
}

func (g GroupRef) filter() string {
	f := groupKinds[g.Kind]
	return strings.TrimSpace(f.key + ":=" + strconv.Quote(g.ID.String()) + " " + f.extra)
}

type LogGroup struct {
	Kind    GroupKind
	ID      uuid.UUID
	Name    string
	Streams int
	LastAt  time.Time
}

type LogStream struct {
	Name   string
	LastAt time.Time
}

type QueryResult struct {
	Columns []string
	Rows    []map[string]string
	Stats   bool
}

func orgFilter(org uuid.UUID) string { return "observatory.org:=" + strconv.Quote(org.String()) }

// LogGroups lists the org's VMs, nodes and images with logs since since, newest entry first.
func (c *Client) LogGroups(ctx context.Context, org uuid.UUID, since time.Time) ([]LogGroup, error) {
	out := []LogGroup{}
	for _, kind := range groupOrder {
		f := groupKinds[kind]
		form := url.Values{
			"query":         {"* | stats by (" + f.key + ") max(_time) last, count_uniq(" + f.stream + ") streams, row_max(_time, " + f.name + ") name"},
			"extra_filters": {strings.TrimSpace(orgFilter(org) + " " + f.key + ":* " + f.extra)},
			"start":         {since.UTC().Format(time.RFC3339Nano)},
		}
		res, err := c.logsQuery(ctx, form)
		if err != nil {
			return nil, err
		}
		for _, row := range res.Rows {
			id, err := uuid.Parse(row[f.key])
			if err != nil {
				continue
			}
			// row_max yields the chosen fields as a JSON object.
			var name map[string]string
			_ = json.Unmarshal([]byte(row["name"]), &name)
			streams, _ := strconv.Atoi(row["streams"])
			last, _ := time.Parse(time.RFC3339Nano, row["last"])
			out = append(out, LogGroup{Kind: kind, ID: id, Name: name[f.name], Streams: streams, LastAt: last})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out, nil
}

// LogGroupStreams lists a group's streams with logs since since, newest entry first.
func (c *Client) LogGroupStreams(ctx context.Context, org uuid.UUID, g GroupRef, since time.Time) ([]LogStream, error) {
	f, ok := groupKinds[g.Kind]
	if !ok {
		return nil, fmt.Errorf("telemetry: unknown log group kind %q", g.Kind)
	}
	res, err := c.logsQuery(ctx, url.Values{
		"query":         {"* | stats by (" + f.stream + ") max(_time) last"},
		"extra_filters": {orgFilter(org) + " " + g.filter()},
		"start":         {since.UTC().Format(time.RFC3339Nano)},
	})
	if err != nil {
		return nil, err
	}
	out := []LogStream{}
	for _, row := range res.Rows {
		last, _ := time.Parse(time.RFC3339Nano, row["last"])
		out = append(out, LogStream{Name: row[f.stream], LastAt: last})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out, nil
}

// GroupLogQuery scopes a LogQuery to one stream of g, or to all its streams when stream is empty; image streams are build ids, which the caller validates.
func (c *Client) GroupLogQuery(org uuid.UUID, g GroupRef, stream string) LogQuery {
	q := LogQuery{Org: org}
	switch g.Kind {
	case KindVM:
		q.VM, q.Stream = g.ID, stream
	case KindNode:
		q.Node, q.Source = g.ID, stream
	case KindImage:
		q.Image = g.ID
		q.Build, _ = uuid.Parse(stream)
	}
	return q
}

// ExplorerQuery runs user LogsQL confined to the org and, when groups is non-empty, to those groups.
func (c *Client) ExplorerQuery(ctx context.Context, org uuid.UUID, groups []GroupRef, q string, from, to time.Time, limit int) (QueryResult, error) {
	if DeniedLogsQL(url.Values{"query": {q}}) {
		return QueryResult{}, &RejectedError{Status: 400, Msg: "query uses a LogsQL construct that is not allowed"}
	}
	if limit <= 0 || limit > MaxExplorerRows {
		limit = MaxExplorerRows
	}
	scope := orgFilter(org)
	ids := map[GroupKind][]string{}
	for _, g := range groups {
		if _, ok := groupKinds[g.Kind]; !ok {
			return QueryResult{}, fmt.Errorf("telemetry: unknown log group kind %q", g.Kind)
		}
		ids[g.Kind] = append(ids[g.Kind], strconv.Quote(g.ID.String()))
	}
	var or []string
	for _, kind := range groupOrder {
		if len(ids[kind]) == 0 {
			continue
		}
		f := groupKinds[kind]
		clause := f.key + ":in(" + strings.Join(ids[kind], ",") + ")"
		if f.extra != "" {
			clause = "(" + clause + " " + f.extra + ")"
		}
		or = append(or, clause)
	}
	if len(or) > 0 {
		scope += " (" + strings.Join(or, " or ") + ")"
	}
	res, err := c.logsQuery(ctx, url.Values{
		"query":         {q},
		"extra_filters": {scope},
		"start":         {from.UTC().Format(time.RFC3339Nano)},
		"end":           {to.UTC().Format(time.RFC3339Nano)},
		"limit":         {strconv.Itoa(limit)},
	})
	if err != nil {
		return QueryResult{}, err
	}
	res.Stats = true
	for _, row := range res.Rows {
		if _, ok := row["_msg"]; ok {
			res.Stats = false
			break
		}
	}
	if !res.Stats {
		// The store's row order under a limit is not guaranteed; RFC3339 times in UTC sort as strings.
		sort.SliceStable(res.Rows, func(i, j int) bool { return res.Rows[i]["_time"] > res.Rows[j]["_time"] })
	}
	return res, nil
}

// logsQuery is bounded by ctx alone, like queryStore; Columns keep the store's field order.
func (c *Client) logsQuery(ctx context.Context, form url.Values) (QueryResult, error) {
	setStoreTimeout(ctx, form)
	resp, err := c.postForm(ctx, c.StoreClient(0), c.LogsURL+"/select/logsql/query", form)
	if err != nil {
		return QueryResult{}, err
	}
	defer resp.Body.Close()
	res := QueryResult{Columns: []string{}, Rows: []map[string]string{}}
	seen := map[string]bool{}
	dec := json.NewDecoder(resp.Body)
	for {
		row, keys, err := decodeRow(dec)
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return QueryResult{}, fmt.Errorf("%w: decode logs: %v", ErrStore, err)
		}
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				res.Columns = append(res.Columns, k)
			}
		}
		res.Rows = append(res.Rows, row)
	}
}

// decodeRow returns io.EOF only between rows; a row cut short is an error.
func decodeRow(dec *json.Decoder) (map[string]string, []string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	row, keys, err := decodeRowBody(dec, tok)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return row, keys, err
}

func decodeRowBody(dec *json.Decoder, tok json.Token) (map[string]string, []string, error) {
	if tok != json.Delim('{') {
		return nil, nil, fmt.Errorf("unexpected %v", tok)
	}
	row := map[string]string{}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		key, _ := k.(string)
		s, ok := v.(string)
		if !ok {
			b, _ := json.Marshal(v)
			s = string(b)
		}
		row[key] = s
		keys = append(keys, key)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	return row, keys, nil
}
