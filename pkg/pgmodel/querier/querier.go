// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package querier

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgtype"
	"github.com/jackc/pgx/v4"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/timestamp"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/timescale/promscale/pkg/log"
	"github.com/timescale/promscale/pkg/pgmodel/cache"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
	"github.com/timescale/promscale/pkg/pgmodel/common/schema"
	"github.com/timescale/promscale/pkg/pgmodel/lreader"
	"github.com/timescale/promscale/pkg/pgmodel/model"
	"github.com/timescale/promscale/pkg/pgxconn"
	"github.com/timescale/promscale/pkg/prompb"
	"github.com/timescale/promscale/pkg/tenancy"
)

// Reader reads the data based on the provided read request.
type Reader interface {
	Read(*prompb.ReadRequest) (*prompb.ReadResponse, error)
}

// Querier queries the data using the provided query data and returns the
// matching timeseries.
type Querier interface {
	// Query returns resulting timeseries for a query.
	Query(*prompb.Query) ([]*prompb.TimeSeries, error)
	// Select returns a series set containing the samples that matches the supplied query parameters.
	Select(mint, maxt int64, sortSeries bool, hints *storage.SelectHints, path []parser.Node, ms ...*labels.Matcher) (storage.SeriesSet, parser.Node)
	// Exemplar returns a exemplar querier.
	Exemplar(ctx context.Context) ExemplarQuerier
}

// ExemplarQuerier queries data using the provided query data and returns the
// matching exemplars.
type ExemplarQuerier interface {
	// Select returns a series set containing the exemplar that matches the supplied query parameters.
	Select(start, end time.Time, ms ...[]*labels.Matcher) ([]model.ExemplarQueryResult, error)
}

const (
	getSampleMetricTableSQL   = "SELECT table_name FROM " + schema.Catalog + ".get_metric_table_name_if_exists($1)"
	getExemplarMetricTableSQL = "SELECT COALESCE(table_name, '') FROM " + schema.Catalog + ".exemplar WHERE metric_name=$1"
)

type pgxQuerier struct {
	conn             pgxconn.PgxConn
	metricTableNames cache.MetricCache
	exemplarPosCache cache.PositionCache
	labelsReader     lreader.LabelsReader
	rAuth            tenancy.ReadAuthorizer
}

// NewQuerier returns a new pgxQuerier that reads from PostgreSQL using PGX
// and caches metric table names and label sets using the supplied caches.
func NewQuerier(
	conn pgxconn.PgxConn,
	metricCache cache.MetricCache,
	labelsReader lreader.LabelsReader,
	exemplarCache cache.PositionCache,
	rAuth tenancy.ReadAuthorizer,
) Querier {
	querier := &pgxQuerier{
		conn:             conn,
		labelsReader:     labelsReader,
		metricTableNames: metricCache,
		exemplarPosCache: exemplarCache,
		rAuth:            rAuth,
	}
	return querier
}

type metricTimeRangeFilter struct {
	metric    string
	startTime string
	endTime   string
}

var _ Querier = (*pgxQuerier)(nil)

// Select implements the Querier interface.
func (q *pgxQuerier) Select(mint, maxt int64, _ bool, hints *storage.SelectHints, path []parser.Node, ms ...*labels.Matcher) (storage.SeriesSet, parser.Node) {
	rows, topNode, err := q.getResultRows(schema.Data, mint, maxt, hints, path, ms)
	if err != nil {
		return errorSeriesSet{err: err}, nil
	}

	ss := buildSeriesSet(rows.([]seriesRow), q.labelsReader)
	return ss, topNode
}

func (q *pgxQuerier) Exemplar(ctx context.Context) ExemplarQuerier {
	return newExemplarQuerier(ctx, q.conn, q, q.metricTableNames, q.exemplarPosCache, q.rAuth)
}

type pgxExemplarQuerier struct {
	conn                pgxconn.PgxConn
	ctx                 context.Context
	metricTableCache    cache.MetricCache
	exemplarKeyPosCache cache.PositionCache
	rAuth               tenancy.ReadAuthorizer
	pgx                 *pgxQuerier // We need a reference to pgxQuerier to reuse the existing functions.
}

// newExemplarQuerier returns a querier that can query over exemplars.
func newExemplarQuerier(
	ctx context.Context,
	conn pgxconn.PgxConn,
	pgx *pgxQuerier,
	metricCache cache.MetricCache,
	exemplarCache cache.PositionCache,
	rAuth tenancy.ReadAuthorizer,
) ExemplarQuerier {
	return &pgxExemplarQuerier{
		ctx:                 ctx,
		conn:                conn,
		pgx:                 pgx,
		metricTableCache:    metricCache,
		exemplarKeyPosCache: exemplarCache,
		rAuth:               rAuth,
	}
}

func (eq *pgxExemplarQuerier) Select(start, end time.Time, matchersList ...[]*labels.Matcher) ([]model.ExemplarQueryResult, error) {
	var (
		numMatchers = len(matchersList)
		results     = make([]model.ExemplarQueryResult, 0, numMatchers)
		rowsCh      = make(chan interface{}, numMatchers)
	)
	for _, matchers := range matchersList {
		go func(m []*labels.Matcher) {
			rows, _, err := eq.pgx.getResultRows(schema.Exemplar, timestamp.FromTime(start), timestamp.FromTime(end), nil, nil, m)
			if rows == nil {
				// Result does not exists.
				rowsCh <- nil
				return
			}
			if err != nil {
				rowsCh <- err
				return
			}
			rowsCh <- rows
		}(matchers)
	}

	// Listen to responses.
	var err error
	shouldProceed := func() bool {
		// Append rows as long as the error is nil. Once we have an error,
		// appending response is just wasting memory as there is no use of it.
		return err == nil
	}
	for i := 0; i < numMatchers; i++ {
		resp := <-rowsCh
		switch out := resp.(type) {
		case exemplarSeriesRow:
			if !shouldProceed() {
				continue
			}
			exemplars, prepareErr := prepareExemplarQueryResult(eq.conn, eq.pgx.labelsReader, eq.exemplarKeyPosCache, out)
			if prepareErr != nil {
				err = prepareErr
				continue
			}
			results = append(results, exemplars)
		case []exemplarSeriesRow:
			if !shouldProceed() {
				continue
			}
			for _, seriesRow := range out {
				exemplars, prepareErr := prepareExemplarQueryResult(eq.conn, eq.pgx.labelsReader, eq.exemplarKeyPosCache, seriesRow)
				if prepareErr != nil {
					err = prepareErr
					continue
				}
				results = append(results, exemplars)
			}
		case error:
			if !shouldProceed() {
				continue
			}
			err = out // Only capture the first error.
		case nil:
		}
	}
	if err != nil {
		return results, fmt.Errorf("selecting exemplars: %w", err)
	}
	sort.Slice(results, func(i, j int) bool {
		return labels.Compare(results[i].SeriesLabels, results[j].SeriesLabels) < 0
	})
	return results, nil
}

// Query implements the Querier interface. It is the entry point for
// remote-storage queries.
func (q *pgxQuerier) Query(query *prompb.Query) ([]*prompb.TimeSeries, error) {
	if query == nil {
		return []*prompb.TimeSeries{}, nil
	}

	matchers, err := fromLabelMatchers(query.Matchers)
	if err != nil {
		return nil, err
	}

	rows, _, err := q.getResultRows(schema.Data, query.StartTimestampMs, query.EndTimestampMs, nil, nil, matchers)
	if err != nil {
		return nil, err
	}

	results, err := buildTimeSeries(rows.([]seriesRow), q.labelsReader)
	return results, err
}

// fromLabelMatchers parses protobuf label matchers to Prometheus label matchers.
func fromLabelMatchers(matchers []*prompb.LabelMatcher) ([]*labels.Matcher, error) {
	result := make([]*labels.Matcher, 0, len(matchers))
	for _, matcher := range matchers {
		var mtype labels.MatchType
		switch matcher.Type {
		case prompb.LabelMatcher_EQ:
			mtype = labels.MatchEqual
		case prompb.LabelMatcher_NEQ:
			mtype = labels.MatchNotEqual
		case prompb.LabelMatcher_RE:
			mtype = labels.MatchRegexp
		case prompb.LabelMatcher_NRE:
			mtype = labels.MatchNotRegexp
		default:
			return nil, fmt.Errorf("invalid matcher type")
		}
		matcher, err := labels.NewMatcher(mtype, matcher.Name, matcher.Value)
		if err != nil {
			return nil, err
		}
		result = append(result, matcher)
	}
	return result, nil
}

type seriesRow struct {
	labelIds []int64
	times    pgtype.TimestamptzArray
	values   pgtype.Float8Array
	err      error
}

type exemplarSeriesRow struct {
	metricName string
	labelIds   []int64
	data       []exemplarRow
}

type exemplarRow struct {
	time        time.Time
	value       float64
	labelValues []string // Exemplar label values.
}

// getResultRows fetches the result row datasets from the database using the
// supplied query parameters.
func (q *pgxQuerier) getResultRows(tableSchema string, startTimestamp int64, endTimestamp int64, hints *storage.SelectHints, path []parser.Node, matchers []*labels.Matcher) (interface{}, parser.Node, error) {
	if q.rAuth != nil {
		matchers = q.rAuth.AppendTenantMatcher(matchers)
	}
	// Build a subquery per metric matcher.
	builder, err := BuildSubQueries(matchers)
	if err != nil {
		return nil, nil, err
	}

	metric := builder.GetMetricName()
	filter := metricTimeRangeFilter{
		metric:    metric,
		startTime: toRFC3339Nano(startTimestamp),
		endTime:   toRFC3339Nano(endTimestamp),
	}

	// If all metric matchers match on a single metric (common case),
	// we query only that single metric.
	if metric != "" {
		clauses, values, err := builder.Build(false)
		if err != nil {
			return nil, nil, err
		}
		return q.querySingleMetric(tableSchema, metric, filter, clauses, values, hints, path)
	}

	clauses, values, err := builder.Build(true)
	if err != nil {
		return nil, nil, err
	}
	return q.queryMultipleMetrics(tableSchema, filter, clauses, values)
}

// querySingleMetric returns all the result rows for a single metric using the
// supplied query parameters. It uses the hints and node path to try to push
// down query functions where possible.
func (q *pgxQuerier) querySingleMetric(tableSchema, metric string, filter metricTimeRangeFilter, cases []string, values []interface{}, hints *storage.SelectHints, path []parser.Node) (interface{}, parser.Node, error) {
	tableName, err := q.getMetricTableName(metric, tableSchema)
	if err != nil {
		// If the metric table is missing, there are no results for this query.
		if err == errors.ErrMissingTableName {
			return nil, nil, nil
		}

		return nil, nil, err
	}
	filter.metric = tableName

	sqlQuery, values, topNode, err := buildTimeseriesByLabelClausesQuery(tableSchema, filter, cases, values, hints, path)
	if err != nil {
		return nil, nil, err
	}

	fmt.Println("sqlQuery", sqlQuery, values)
	rows, err := q.conn.Query(context.Background(), sqlQuery, values...)
	if err != nil {
		// If we are getting undefined table error, it means the query
		// is looking for a metric which doesn't exist in the system.
		if e, ok := err.(*pgconn.PgError); !ok || e.Code != pgerrcode.UndefinedTable {
			return nil, nil, err
		}
	}

	defer rows.Close()

	// TODO this allocation assumes we usually have 1 row, if not, refactor
	switch tableSchema {
	case schema.Data:
		samplesRows, err := appendSampleRows(make([]seriesRow, 0, 1), rows)
		if err != nil {
			return nil, topNode, fmt.Errorf("appending sample rows: %w", err)
		}
		return samplesRows, topNode, nil
	case schema.Exemplar:
		exemplarSeriesRows, err := appendExemplarRows(metric, rows)
		if err != nil {
			return nil, topNode, fmt.Errorf("appending exemplar rows: %w", err)
		}
		return exemplarSeriesRows, nil, nil
	default:
		panic("invalid schema")
	}
}

// queryMultipleMetrics returns all the result rows for across multiple metrics
// using the supplied query parameters.
func (q *pgxQuerier) queryMultipleMetrics(tableSchema string, filter metricTimeRangeFilter, cases []string, values []interface{}) (interface{}, parser.Node, error) {
	// First fetch series IDs per metric.
	sqlQuery := BuildMetricNameSeriesIDQuery(cases)
	rows, err := q.conn.Query(context.Background(), sqlQuery, values...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	metrics, series, err := GetSeriesPerMetric(rows)
	if err != nil {
		return nil, nil, err
	}

	// TODO this assume on average on row per-metric. Is this right?
	var (
		results          interface{}
		metricTableNames = make([]string, 0, len(metrics)) // This will be required to fill the `metricName` field of exemplarResult.
	)
	switch tableSchema {
	case schema.Data:
		results = make([]seriesRow, 0, len(metrics))
	case schema.Exemplar:
		results = make([]exemplarSeriesRow, 0, len(metrics))
	default:
		panic("invalid type")
	}

	numQueries := 0
	batch := q.conn.NewBatch()

	// Generate queries for each metric and send them in a single batch.
	for i, metric := range metrics {
		//TODO batch getMetricTableName
		tableName, err := q.getMetricTableName(metric, tableSchema)
		if err != nil {
			// If the metric table is missing, there are no results for this query.
			if err == errors.ErrMissingTableName {
				continue
			}
			return nil, nil, err
		}
		filter.metric = tableName
		sqlQuery, err := buildTimeseriesBySeriesIDQuery(tableSchema, filter, series[i])
		fmt.Println(sqlQuery)
		if err != nil {
			return nil, nil, fmt.Errorf("build timeseries by series-id: %w", err)
		}
		metricTableNames = append(metricTableNames, tableName)
		batch.Queue(sqlQuery)
		numQueries += 1
	}

	batchResults, err := q.conn.SendBatch(context.Background(), batch)
	if err != nil {
		return nil, nil, err
	}
	defer batchResults.Close()

	for i := 0; i < numQueries; i++ {
		rows, err = batchResults.Query()
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		// Append all rows into results.
		if tableSchema == schema.Data {
			// If the query is for fetching samples.
			results, err = appendSampleRows(results.([]seriesRow), rows)
			rows.Close()
			if err != nil {
				rows.Close()
				return nil, nil, err
			}
			continue
		}
		metricTable := metricTableNames[i]
		seriesRows, err := appendExemplarRows(metricTable, rows)
		if err != nil {
			return nil, nil, fmt.Errorf("append exemplar rows: %w", err)
		}
		exemplarResults := results.([]exemplarSeriesRow)
		exemplarResults = append(exemplarResults, seriesRows...)
		results = exemplarResults
		// Can't defer because we need to Close before the next loop iteration.
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}

	return results, nil, nil
}

func labelIdsMatch(a, b []int64) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// getMetricTableName gets the table name for a specific metric from internal
// cache. If not found, fetches it from the database and updates the cache.
func (q *pgxQuerier) getMetricTableName(metric, tableSchema string) (string, error) {
	var (
		isExemplar      bool
		tableFetchQuery = getSampleMetricTableSQL
	)
	if tableSchema == schema.Exemplar {
		// The incoming query is for exemplar data. Let's change our parameters
		// so that the operations with the database and cache is focused towards
		// exemplars.
		isExemplar = true
		tableFetchQuery = getExemplarMetricTableSQL
	}

	tableName, err := q.getTableName(metric, isExemplar)
	if err == nil {
		return tableName, nil
	}
	if err != errors.ErrEntryNotFound {
		return "", err
	}

	tableName, err = q.queryMetricTableName(metric, tableFetchQuery)
	if err != nil {
		return "", err
	}

	err = q.metricTableNames.Set(metric, tableName, isExemplar)
	return tableName, err
}

func (q *pgxQuerier) getTableName(metric string, isExemplar bool) (string, error) {
	var (
		err       error
		tableName string
	)

	tableName, err = q.metricTableNames.Get(metric, isExemplar)
	if err == nil {
		return tableName, nil
	}
	return "", err
}

// queryMetricTableName returns table name for the given metric by evaluating the query passed as param.
// If you want the table name to be for samples, pass getSampleMetricTableSQL as query.
// If you want the table name to be for exemplars, pass getExemplarMetricTableSQL as query.
func (q *pgxQuerier) queryMetricTableName(metric, query string) (string, error) {
	res, err := q.conn.Query(context.Background(), query, metric)
	if err != nil {
		return "", err
	}

	var tableName string
	defer res.Close()

	if !res.Next() {
		return "", errors.ErrMissingTableName
	}

	if err := res.Scan(&tableName); err != nil {
		return "", err
	}

	return tableName, nil
}

// appendTsRows adds new results rows to already existing result rows and
// returns the as a result.
func appendSampleRows(out []seriesRow, in pgx.Rows) ([]seriesRow, error) {
	if in.Err() != nil {
		return out, in.Err()
	}
	for in.Next() {
		var row seriesRow
		row.err = in.Scan(&row.labelIds, &row.times, &row.values)
		out = append(out, row)
		if row.err != nil {
			return out, row.err
		}
	}
	return out, in.Err()
}

// appendExemplarRows adds new results rows to already existing result rows and
// returns the as a result.
func appendExemplarRows(metricName string, in pgx.Rows) (rows []exemplarSeriesRow, err error) {
	if in.Err() != nil {
		return rows, in.Err()
	}
	seriesRowMap := make(map[string]*exemplarSeriesRow)
	// Note: The rows of multiple exemplar query, are such that `rows` here contains different series, represented by
	// different label_ids array, that satisfy the given labels.
	for in.Next() {
		var (
			err      error
			row      exemplarRow
			labelIds []int64
		)
		err = in.Scan(&labelIds, &row.time, &row.value, &row.labelValues)
		if err != nil {
			log.Error("err", err)
			return rows, err
		}

		key := fmt.Sprintf("%s", labelIds)
		if existingSeriesRow, exists := seriesRowMap[key]; exists {
			existingSeriesRow.data = append(existingSeriesRow.data, row)
			continue
		}
		// New exemplar series.
		seriesRow := &exemplarSeriesRow{
			metricName: metricName,
			labelIds:   labelIds,
			data:       make([]exemplarRow, 0, 1),
		}
		seriesRow.data = append(seriesRow.data, row)
		seriesRowMap[key] = seriesRow
	}
	return getExemplarSeriesRows(seriesRowMap), in.Err()
}

func getExemplarSeriesRows(m map[string]*exemplarSeriesRow) []exemplarSeriesRow {
	s := make([]exemplarSeriesRow, len(m))
	i := 0
	for _, v := range m {
		s[i] = *v
		i++
	}
	return s
}

// errorSeriesSet represents an error result in a form of a series set.
// This behavior is inherited from Prometheus codebase.
type errorSeriesSet struct {
	err error
}

func (errorSeriesSet) Next() bool                   { return false }
func (errorSeriesSet) At() storage.Series           { return nil }
func (e errorSeriesSet) Err() error                 { return e.err }
func (e errorSeriesSet) Warnings() storage.Warnings { return nil }

type labelQuerier interface {
	LabelsForIds(ids []int64) (lls labels.Labels, err error)
}
