package tempo

import (
	"context"
	"errors"
	"fmt"
	"github.com/VictoriaMetrics/VictoriaLogs/lib/logstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/timeutil"
	"github.com/VictoriaMetrics/VictoriaTraces/app/vtselect/traces/query"
	"github.com/VictoriaMetrics/VictoriaTraces/app/vtstorage"
	otelpb "github.com/VictoriaMetrics/VictoriaTraces/lib/protoparser/opentelemetry/pb"
	"github.com/VictoriaMetrics/VictoriaTraces/lib/traceql"
	"github.com/VictoriaMetrics/metrics"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	tempoSearchTagsRequests = metrics.NewCounter(`vt_http_requests_total{path="/select/tempo/api/v2/search/tags"}`)
	tempoSearchTagsDuration = metrics.NewSummary(`vt_http_request_duration_seconds{path="/select/tempo/api/v2/search/tags"}`)

	tempoSearchTagValuesRequests = metrics.NewCounter(`vt_http_requests_total{path="/select/tempo/api/v2/search/tag/*/values"}`)
	tempoSearchTagValuesDuration = metrics.NewSummary(`vt_http_request_duration_seconds{path="/select/tempo/api/v2/search/tag/*/values"}`)
)

func RequestHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) bool {
	httpserver.EnableCORS(w, r)
	startTime := time.Now()
	path := r.URL.Path
	if path == "/select/tempo/api/v2/search/tags" {
		tempoSearchTagsRequests.Inc()
		processSearchTagsRequest(ctx, w, r)
		tempoSearchTagsDuration.UpdateDuration(startTime)
		return true
	} else if strings.HasPrefix(path, "/select/tempo/api/v2/search/tag/") && strings.HasSuffix(path, "/values") {
		tempoSearchTagValuesRequests.Inc()
		processSearchTagValuesRequest(ctx, w, r)
		tempoSearchTagValuesDuration.UpdateDuration(startTime)
		return true
	}
	return false
}

// processSearchTagsRequest handle the Tempo /api/v2/search/tags API request.
func processSearchTagsRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	cp, err := query.GetCommonParams(r)
	if err != nil {
		httpserver.Errorf(w, r, "incorrect query params: %s", err)
		return
	}

	params, err := parseTempoAPIParam(ctx, r)
	if err != nil {
		httpserver.Errorf(w, r, "incorrect query params: %s", err)
		return
	}

	q := r.URL.Query()
	scope := q.Get("scope")

	result, err := searchTags(ctx, cp, params.q, scope, params.start.UnixNano(), params.end.UnixNano(), params.limit)
	if err != nil {
		httpserver.Errorf(w, r, "cannot get services list: %s", err)
		return
	}

	// Write results
	w.Header().Set("Content-Type", "application/json")
	WriteSearchTagsResponse(w, result.resourceTagList, result.spanTagList, result.eventTagList, result.linkTagList, result.instrumentationScopeTagList)
}

// processSearchTagValuesRequest handle the Tempo /api/v2/search/tag/*/values API request.
func processSearchTagValuesRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	cp, err := query.GetCommonParams(r)
	if err != nil {
		httpserver.Errorf(w, r, "incorrect query params: %s", err)
		return
	}

	// extract the `tag` name.
	// the path must be like `/select/tempo/api/v2/search/tag/<tag>/values`.
	u := r.URL.Path[len("/select/tempo/api/v2/search/tag/"):]

	// check for invalid path: /select/tempo/api/v2/search/tag/values
	if !strings.Contains(u, "/") {
		httpserver.Errorf(w, r, "incorrect query path [%s]", r.URL.Path)
		return
	}
	tagName := u[:len(u)-len("/values")]
	if len(tagName) == 0 {
		httpserver.Errorf(w, r, "incorrect query path [%s]", r.URL.Path)
		return
	}

	params, err := parseTempoAPIParam(ctx, r)
	if err != nil {
		httpserver.Errorf(w, r, "incorrect query params: %s", err)
		return
	}

	// let's start from basic fields: service name, span name and status first.
	var mappedTagName string
	switch tagName {
	case "resource.service.name", "service.name":
		mappedTagName = otelpb.ResourceAttrServiceName
	case "status":
		mappedTagName = otelpb.StatusCodeField
	case "name":
		mappedTagName = otelpb.NameField
	default:
		httpserver.Errorf(w, r, "unsupported tag name [%s]", tagName)
		return
	}

	result, err := searchTagValues(ctx, cp, params.q, mappedTagName, params.start.UnixNano(), params.end.UnixNano(), params.limit)
	if err != nil {
		httpserver.Errorf(w, r, "cannot get tag values: %s", err)
		return
	}

	// Write results
	w.Header().Set("Content-Type", "application/json")
	WriteSearchTagValuesResponse(w, result)
}

type searchTagResult struct {
	resourceTagList, spanTagList, eventTagList, linkTagList, instrumentationScopeTagList []string
}

func searchTags(ctx context.Context, cp *query.CommonParams, traceQLStr string, scope string, start, end, limit int64) (*searchTagResult, error) {
	// transform traceQL into LogsQL as filter. It should contain filter only without any pipe.
	filterQuery, err := traceql.ParseQuery(traceQLStr)
	if err != nil {
		return nil, err
	}

	scopes := ``
	pipeLimit := limit
	switch scope {
	case "instrumentation":
		scopes = fmt.Sprintf(`| filter name:"%s:"*`, otelpb.InstrumentationScopeAttrPrefix)
	case "resource":
		scopes = fmt.Sprintf(`| filter name:"%s:"*`, otelpb.ResourceAttrPrefix)
	case "span":
		scopes = fmt.Sprintf(`| filter name:"%s:"*`, otelpb.SpanAttrPrefixField)
	case "event":
		scopes = fmt.Sprintf(`| filter name:"%s:"*`, otelpb.EventPrefix+otelpb.EventAttrPrefix)
	case "link":
		scopes = fmt.Sprintf(`| filter name:"%s:"*`, otelpb.LinkPrefix+otelpb.LinkAttrPrefix)
	case "intrinsic":
		return nil, errors.New("scope: intrinsic is not supported yet")
	case "", "all":
		// todo: this does not align with the doc, but user usually don't expect a result fully match the limit
		// because they're not really looking for a specific tag name when no scope argument is used. it's likely
		// to be an initial request when loading possible option on the search page.
		// so returning "something" should be enough.
		pipeLimit = limit * 3
	default:
		return nil, fmt.Errorf("unsupported scope: %s", scope)
	}

	qStr := fmt.Sprintf(`%s | field_names %s | uniq by (name)`,
		filterQuery.String(), scopes,
	)

	q, err := logstorage.ParseQueryAtTimestamp(qStr, time.Now().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("cannot parse query [%s]: %s", qStr, err)
	}
	q.AddTimeFilter(start, end)
	q.AddPipeOffsetLimit(0, uint64(pipeLimit))

	fieldNames, err := singleFieldQueryHelper(ctx, q, cp, pipeLimit)
	if err != nil {
		return nil, fmt.Errorf("cannot parse query [%s]: %s", qStr, err)
	}

	result := &searchTagResult{
		resourceTagList:             []string{},
		spanTagList:                 []string{},
		instrumentationScopeTagList: []string{},
		eventTagList:                []string{},
		linkTagList:                 []string{},
	}
	for i := range fieldNames {
		if strings.HasPrefix(fieldNames[i], otelpb.EventPrefix+otelpb.EventAttrPrefix) {
			lIdx := strings.LastIndex(fieldNames[i], ":")
			result.eventTagList = appendNoExceedN(result.eventTagList, fieldNames[i][len(otelpb.EventPrefix+otelpb.EventAttrPrefix):lIdx], limit)
		} else if strings.HasPrefix(fieldNames[i], otelpb.SpanAttrPrefixField) {
			result.spanTagList = appendNoExceedN(result.spanTagList, fieldNames[i][len(otelpb.SpanAttrPrefixField):], limit)
		} else if strings.HasPrefix(fieldNames[i], otelpb.ResourceAttrPrefix) {
			result.resourceTagList = appendNoExceedN(result.resourceTagList, fieldNames[i][len(otelpb.ResourceAttrPrefix):], limit)
		} else if strings.HasPrefix(fieldNames[i], otelpb.InstrumentationScopeAttrPrefix) {
			result.instrumentationScopeTagList = appendNoExceedN(result.instrumentationScopeTagList, fieldNames[i][len(otelpb.InstrumentationScopeAttrPrefix):], limit)
		} else if strings.HasPrefix(fieldNames[i], otelpb.LinkPrefix+otelpb.LinkAttrPrefix) {
			lIdx := strings.LastIndex(fieldNames[i], ":")
			result.linkTagList = appendNoExceedN(result.linkTagList, fieldNames[i][len(otelpb.LinkPrefix+otelpb.LinkAttrPrefix):lIdx], limit)
		}
	}
	return result, nil
}

func appendNoExceedN(s []string, item string, n int64) []string {
	if len(s) >= int(n) {
		return s
	}
	return append(s, item)
}

func searchTagValues(ctx context.Context, cp *query.CommonParams, traceQLStr, tagName string, start, end, limit int64) ([]string, error) {
	// transform traceQL into LogsQL as filter. It should contain filter only without any pipe.
	filterQuery, err := traceql.ParseQuery(traceQLStr)
	if err != nil {
		return nil, err
	}

	qStr := fmt.Sprintf(`%s | fields %q | field_values %q | fields %q`,
		filterQuery.String(), tagName, tagName, tagName,
	)

	q, err := logstorage.ParseQueryAtTimestamp(qStr, time.Now().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("cannot parse query [%s]: %s", qStr, err)
	}
	q.AddTimeFilter(start, end)
	q.AddPipeOffsetLimit(0, uint64(limit))

	return singleFieldQueryHelper(ctx, q, cp, limit)
}

// singleFieldQueryHelper execute queries which contains only a single field in response, and return as []string.
// it's useful for queries looking for `field_name`s or `field_value`s.
func singleFieldQueryHelper(ctx context.Context, q *logstorage.Query, cp *query.CommonParams, limit int64) ([]string, error) {
	resultList := make([]string, 0, limit)
	writeBlock := func(_ uint, db *logstorage.DataBlock) {
		columns := db.Columns
		if len(columns) != 1 {
			logger.Panicf("BUG: unexpected column(s) returned for singleFieldQueryHelper: %v", columns)
		}

		for _, v := range columns[0].Values {
			if v != "" {
				resultList = append(resultList, strings.Clone(v))
			}
		}
	}

	cp.Query = q
	qctx := cp.NewQueryContext(ctx)
	defer cp.UpdatePerQueryStatsMetrics()

	if err := vtstorage.RunQuery(qctx, writeBlock); err != nil {
		return nil, err
	}

	return resultList, nil
}

type commonAPIParam struct {
	q     string
	start time.Time
	end   time.Time
	limit int64
}

// parseTempoAPIParam parse Tempo request.
func parseTempoAPIParam(_ context.Context, r *http.Request) (*commonAPIParam, error) {
	// default params
	p := &commonAPIParam{
		q:     "{}",
		start: time.Now().Add(-10 * time.Minute),
		end:   time.Now(),
		limit: 100,
	}

	q := r.URL.Query()

	start := q.Get("start")
	if start != "" {
		ts, ok := timeutil.TryParseUnixTimestamp(start)
		if !ok {
			return nil, fmt.Errorf("cannot parse start timestamp: %s", start)
		}
		p.start = time.Unix(ts, 0)
	}
	end := q.Get("end")
	if end != "" {
		ts, ok := timeutil.TryParseUnixTimestamp(end)
		if !ok {
			return nil, fmt.Errorf("cannot parse end timestamp: %s", start)
		}
		p.end = time.Unix(ts, 0)
	}
	if p.start.After(p.end) {
		p.start = p.end.Add(-10 * time.Minute)
	}

	limit := q.Get("limit")
	if limit != "" {
		l, err := strconv.ParseInt(limit, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse limit: %s", limit)
		}
		p.limit = l
	}

	qStr := q.Get("q")
	if qStr != "" {
		_, err := traceql.ParseQuery(qStr)
		if err != nil {
			return nil, fmt.Errorf("cannot parse q: %s", qStr)
		}
		p.q = qStr
	}

	return p, nil
}
