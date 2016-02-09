package appdash

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"time"

	influxDBClient "github.com/influxdb/influxdb/client"
	influxDBServer "github.com/influxdb/influxdb/cmd/influxd/run"
	influxDBModels "github.com/influxdb/influxdb/models"
)

const (
	dbName               string = "appdash" // InfluxDB db name.
	spanMeasurementName  string = "spans"   // InfluxDB container name for trace spans.
	defaultTracesPerPage int    = 10        // Default number of traces per page.
)

// Compile-time "implements" check.
var _ interface {
	Store
	Queryer
} = (*InfluxDBStore)(nil)

// zeroID is ID's zero value as string.
var zeroID string = ID(0).String()

// pointFields -> influxDBClient.Point.Fields
type pointFields map[string]interface{}

type InfluxDBStore struct {
	con           *influxDBClient.Client // InfluxDB client connection.
	server        *influxDBServer.Server // InfluxDB API server.
	tracesPerPage int                    // Number of traces per page.
}

func (in *InfluxDBStore) Collect(id SpanID, anns ...Annotation) error {
	p, err := in.findSpanPoint(id)
	if err != nil {
		return err
	}

	// trace_id, span_id & parent_id are set as tags
	// because InfluxDB tags are indexed & those values
	// are used later on queries.
	tags := map[string]string{
		"trace_id":  id.Trace.String(),
		"span_id":   id.Span.String(),
		"parent_id": id.Parent.String(),
	}

	// Saving annotations as InfluxDB measurement spans fields
	// which are not indexed.
	fields := make(map[string]interface{}, len(anns))
	for _, ann := range anns {
		fields[ann.Key] = string(ann.Value)
	}

	if p != nil { // span exists on DB.
		p.Measurement = spanMeasurementName
		p.Tags = tags
		// Using extendFields & withoutEmptyFields in order to have
		// pointFields that only contain:
		// - Fields that are not saved on DB.
		// - Fields that are saved but have empty values.
		p.Fields = extendFields(fields, withoutEmptyFields(p.Fields))
	} else { // new span to be saved on DB.
		p = &influxDBClient.Point{
			Measurement: spanMeasurementName,
			Tags:        tags,   // indexed metadata.
			Fields:      fields, // non-indexed metadata.
			Time:        time.Now().UTC(),
		}
	}

	// InfluxDB point represents a single span.
	pts := []influxDBClient.Point{*p}
	bps := influxDBClient.BatchPoints{
		Points:          pts,
		Database:        dbName,
		RetentionPolicy: "default",
	}
	_, writeErr := in.con.Write(bps)
	if writeErr != nil {
		return writeErr
	}
	return nil
}

func (in *InfluxDBStore) Trace(id ID) (*Trace, error) {
	trace := &Trace{}

	// GROUP BY * -> meaning group by all tags(trace_id, span_id & parent_id)
	// grouping by all tags includes those and it's values on the query response.
	q := fmt.Sprintf("SELECT * FROM spans WHERE trace_id='%s' GROUP BY *", id)
	result, err := in.executeOneQuery(q)
	if err != nil {
		return nil, err
	}

	// result.Series -> A slice containing all the spans.
	if len(result.Series) == 0 {
		return nil, errors.New("trace not found")
	}

	// Iterate over series(spans) to create trace children's & set trace fields.
	var rootSpanSet bool
	for _, s := range result.Series {
		var isRootSpan bool
		span, err := newSpanFromRow(&s)
		if err != nil {
			return nil, err
		}
		annotations, err := annotationsFromRow(&s)
		if err != nil {
			return trace, nil
		}
		span.Annotations = *annotations
		if span.ID.IsRoot() && rootSpanSet {
			return nil, errors.New("unexpected multiple root spans")
		}
		if span.ID.IsRoot() && !rootSpanSet {
			isRootSpan = true
		}
		if isRootSpan { // root span.
			trace.Span = *span
			rootSpanSet = true
		} else { // children span.
			trace.Sub = append(trace.Sub, &Trace{Span: *span})
		}
	}
	return trace, nil
}

func (in *InfluxDBStore) Traces() ([]*Trace, error) {
	traces := make([]*Trace, 0)

	// GROUP BY * -> meaning group by all tags(trace_id, span_id & parent_id)
	// grouping by all tags includes those and it's values on the query response.
	rootSpansQuery := fmt.Sprintf("SELECT * FROM spans WHERE parent_id='%s' GROUP BY * LIMIT %d", zeroID, in.tracesPerPage)
	rootSpansResult, err := in.executeOneQuery(rootSpansQuery)
	if err != nil {
		return nil, err
	}

	// result.Series -> A slice containing all the spans.
	if len(rootSpansResult.Series) == 0 {
		return traces, nil
	}

	// Cache to keep track of traces to be returned.
	tracesCache := make(map[ID]*Trace, 0)

	// Iterate over series(spans) to create traces.
	for _, s := range rootSpansResult.Series {
		span, err := newSpanFromRow(&s)
		if err != nil {
			return nil, err
		}
		annotations, err := annotationsFromRow(&s)
		if err != nil {
			return nil, err
		}
		span.Annotations = *annotations
		_, present := tracesCache[span.ID.Trace]
		if !present {
			tracesCache[span.ID.Trace] = &Trace{Span: *span}
		} else {
			return nil, errors.New("duplicated root span")
		}
	}

	// Using 'OR' since 'IN' not supported yet.
	where := `WHERE `
	var i int = 1
	for _, trace := range tracesCache {
		where += fmt.Sprintf("(trace_id='%s' AND parent_id!='%s')", trace.Span.ID.Trace, zeroID)

		// Adds 'OR' except for last iteration.
		if i != len(tracesCache) && len(tracesCache) > 1 {
			where += " OR "
		}
		i += 1
	}

	// Queries for all children spans of the traces to be returned.
	childrenSpansQuery := fmt.Sprintf("SELECT * FROM spans %s GROUP BY *", where)
	childrenSpansResult, err := in.executeOneQuery(childrenSpansQuery)
	if err != nil {
		return nil, err
	}

	// Iterate over series(children spans) to create sub-traces
	// and associates sub-traces with it's parent trace.
	for _, s := range childrenSpansResult.Series {
		span, err := newSpanFromRow(&s)
		if err != nil {
			return nil, err
		}
		annotations, err := annotationsFromRow(&s)
		if err != nil {
			return nil, err
		}
		span.Annotations = *annotations
		trace, present := tracesCache[span.ID.Trace]
		if !present { // Root trace not added.
			return nil, errors.New("parent not found")
		} else { // Root trace already added so append a sub-trace.
			trace.Sub = append(trace.Sub, &Trace{Span: *span})
		}
	}
	for _, trace := range tracesCache {
		traces = append(traces, trace)
	}
	return traces, nil
}

func (in *InfluxDBStore) Close() error {
	return in.server.Close()
}

func (in *InfluxDBStore) createDBIfNotExists() error {
	// If no errors query execution was successfully - either DB was created or already exists.
	response, err := in.con.Query(influxDBClient.Query{
		Command: fmt.Sprintf("%s %s", "CREATE DATABASE IF NOT EXISTS", dbName),
	})
	if err != nil {
		return err
	}
	if response.Error() != nil {
		return response.Error()
	}
	return nil
}

func (in *InfluxDBStore) executeOneQuery(command string) (*influxDBClient.Result, error) {
	response, err := in.con.Query(influxDBClient.Query{
		Command:  command,
		Database: dbName,
	})
	if err != nil {
		return nil, err
	}
	if response.Error() != nil {
		return nil, response.Error()
	}

	// Expecting one result, since a single query is executed.
	if len(response.Results) != 1 {
		return nil, errors.New("unexpected number of results for an influxdb single query")
	}
	return &response.Results[0], nil
}

func (in *InfluxDBStore) findSpanPoint(ID SpanID) (*influxDBClient.Point, error) {
	q := fmt.Sprintf(`
		SELECT * FROM spans WHERE trace_id='%s' AND span_id='%s' AND parent_id='%s' GROUP BY *
	`, ID.Trace, ID.Span, ID.Parent)
	result, err := in.executeOneQuery(q)
	if err != nil {
		return nil, err
	}
	if len(result.Series) == 0 {
		return nil, nil
	}
	if len(result.Series) > 1 {
		return nil, errors.New("unexpected multiple series")
	}
	r := result.Series[0]
	if len(r.Values) == 0 {
		return nil, errors.New("unexpected empty series")
	}
	p := influxDBClient.Point{
		Fields: make(pointFields, 0),
	}
	fields := r.Values[0]
	for i, field := range fields {
		key := r.Columns[i]
		switch field.(type) {
		case string:
			// time field is set by InfluxDB not related to annotations.
			if key == "time" {
				t, err := time.Parse(time.RFC3339Nano, field.(string))
				if err != nil {
					return nil, err
				}
				p.Time = t
			}
			p.Fields[key] = field.(string)
		case nil:
			continue
		default:
			return nil, fmt.Errorf("unexpected field type: %v", reflect.TypeOf(field))
		}
	}
	return &p, err
}

func (in *InfluxDBStore) init(server *influxDBServer.Server) error {
	in.server = server
	url, err := url.Parse(fmt.Sprintf("http://%s:%d", influxDBClient.DefaultHost, influxDBClient.DefaultPort))
	if err != nil {
		return err
	}
	con, err := influxDBClient.NewClient(influxDBClient.Config{URL: *url})
	if err != nil {
		return err
	}
	in.con = con
	if err := in.createDBIfNotExists(); err != nil {
		return err
	}
	// TODO: support specifying the number of traces per page.
	in.tracesPerPage = defaultTracesPerPage
	return nil
}

func annotationsFromRow(r *influxDBModels.Row) (*Annotations, error) {
	// Actually an influxDBModels.Row represents a single InfluxDB serie.
	// r.Values[n] is a slice containing span's annotation values.
	var fields []interface{}
	if len(r.Values) == 1 {
		fields = r.Values[0]
	}

	// len(r.Values) might be greater than one - meaning there are
	// some spans to drop, see: InfluxDBStore.Collect(...).
	// If so last one is picked.
	if len(r.Values) > 1 {
		fields = r.Values[len(r.Values)-1]
	}
	annotations := make(Annotations, 0)

	// Iterates over fields which represent span's annotation values.
	for i, field := range fields {
		// It is safe to do column[0] (eg. 'Server.Request.Method')
		// matches fields[0] (eg. 'GET')
		key := r.Columns[i]
		var value []byte
		switch field.(type) {
		case string:
			value = []byte(field.(string))
		case nil:
		default:
			return nil, fmt.Errorf("unexpected field type: %v", reflect.TypeOf(field))
		}
		a := Annotation{
			Key:   key,
			Value: value,
		}
		annotations = append(annotations, a)
	}

	return &annotations, nil
}

// extendFields replaces existing items on dst from src.
func extendFields(dst, src pointFields) pointFields {
	for k, v := range src {
		if _, present := dst[k]; present {
			dst[k] = v
		}
	}
	return dst
}

// withoutEmptyFields returns a pointFields without
// those fields that has empty values.
func withoutEmptyFields(pf pointFields) pointFields {
	r := make(pointFields, 0)
	for k, v := range pf {
		switch v.(type) {
		case string:
			if v.(string) == "" {
				continue
			}
			r[k] = v
		case nil:
			continue
		default:
			r[k] = v
		}
	}
	return r
}

func newSpanFromRow(r *influxDBModels.Row) (*Span, error) {
	span := &Span{}
	traceID, err := ParseID(r.Tags["trace_id"])
	if err != nil {
		return nil, err
	}
	spanID, err := ParseID(r.Tags["span_id"])
	if err != nil {
		return nil, err
	}
	parentID, err := ParseID(r.Tags["parent_id"])
	if err != nil {
		return nil, err
	}
	span.ID = SpanID{
		Trace:  ID(traceID),
		Span:   ID(spanID),
		Parent: ID(parentID),
	}
	return span, nil
}

type InfluxDBStoreConfig struct {
	Server    *influxDBServer.Config
	BuildInfo *influxDBServer.BuildInfo
}

func NewInfluxDBStore(config InfluxDBStoreConfig) (*InfluxDBStore, error) {
	//TODO: add Authentication.
	s, err := influxDBServer.NewServer(config.Server, config.BuildInfo)
	if err != nil {
		return nil, err
	}
	if err := s.Open(); err != nil {
		return nil, err
	}
	var in InfluxDBStore
	if err := in.init(s); err != nil {
		return nil, err
	}
	return &in, nil
}
