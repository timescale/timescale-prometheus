// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package ingestor

import (
	"fmt"
	"time"

	"github.com/timescale/promscale/pkg/clockcache"
	"github.com/timescale/promscale/pkg/pgmodel/cache"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
	"github.com/timescale/promscale/pkg/pgmodel/model"
	"github.com/timescale/promscale/pkg/pgxconn"
	"github.com/timescale/promscale/pkg/prompb"
)

type Cfg struct {
	AsyncAcks              bool
	ReportInterval         int
	NumCopiers             int
	DisableEpochSync       bool
	IgnoreCompressedChunks bool
}

// DBIngestor ingest the TimeSeries data into Timescale database.
type DBIngestor struct {
	dispatcher model.Dispatcher
	sCache     cache.SeriesCache
}

// NewPgxIngestor returns a new Ingestor that uses connection pool and a metrics cache
// for caching metric table names.
func NewPgxIngestor(conn pgxconn.PgxConn, cache cache.MetricCache, sCache cache.SeriesCache, cfg *Cfg) (*DBIngestor, error) {
	dispatcher, err := newPgxDispatcher(conn, cache, sCache, cfg)
	if err != nil {
		return nil, err
	}

	return &DBIngestor{dispatcher: dispatcher, sCache: sCache}, nil
}

// NewPgxIngestorForTests returns a new Ingestor that write to PostgreSQL using PGX
// with an empty config, a new default size metrics cache and a non-ha-aware data parser
func NewPgxIngestorForTests(conn pgxconn.PgxConn, cfg *Cfg) (*DBIngestor, error) {
	if cfg == nil {
		cfg = &Cfg{}
	}
	c := &cache.MetricNameCache{Metrics: clockcache.WithMax(cache.DefaultMetricCacheSize)}
	s := cache.NewSeriesCache(cache.DefaultConfig, nil)
	return NewPgxIngestor(conn, c, s, cfg)
}

func (ingestor *DBIngestor) samples(l *model.Series, ts *prompb.TimeSeries) (model.Insertable, int, error) {
	return model.NewInsertable(l, ts.Samples), len(ts.Samples), nil
}

func (ingestor *DBIngestor) exemplars(l *model.Series, ts *prompb.TimeSeries) (model.Insertable, int, error) {
	return model.NewInsertable(l, ts.Exemplars), len(ts.Exemplars), nil
}

// Ingest transforms and ingests the timeseries data into Timescale database.
// input:
//     tts the []Timeseries to insert
//     req the WriteRequest backing tts. It will be added to our WriteRequest
//         pool when it is no longer needed.
func (ingestor *DBIngestor) Ingest(r *prompb.WriteRequest) (uint64, error) {
	var (
		err       error
		totalRows uint64

		dataSamples = make(map[string][]model.Insertable)
	)
	for i := range r.Timeseries {
		var (
			ts         = &r.Timeseries[i]
			series     *model.Series
			metricName string
			err        error
		)
		if len(ts.Labels) > 0 {
			// Normalize and canonicalize t.Labels.
			// After this point t.Labels should never be used again.
			series, metricName, err = ingestor.sCache.GetSeriesFromProtos(ts.Labels)
			if err != nil {
				return 0, err
			}
			if metricName == "" {
				return 0, errors.ErrNoMetricName
			}
		}

		if len(ts.Samples) > 0 {
			samples, count, err := ingestor.samples(series, ts)
			if err != nil {
				return 0, fmt.Errorf("samples: %w", err)
			}
			totalRows += uint64(count)
			dataSamples[metricName] = append(dataSamples[metricName], samples)
		}
		if len(ts.Exemplars) > 0 {
			exemplars, count, err := ingestor.exemplars(series, ts)
			if err != nil {
				return 0, fmt.Errorf("exemplars: %w", err)
			}
			totalRows += uint64(count)
			dataSamples[metricName] = append(dataSamples[metricName], exemplars)
		}
		// we're going to free req after this, but we still need the samples,
		// so nil the field
		ts.Samples = nil
		ts.Exemplars = nil
	}
	// WriteRequests can contain pointers into the original buffer we deserialized
	// them out of, and can be quite large in and of themselves. In order to prevent
	// memory blowup, and to allow faster deserializing, we recycle the WriteRequest
	// here, allowing it to be either garbage collected or reused for a new request.
	// In order for this to work correctly, any data we wish to keep using (e.g.
	// samples) must no longer be reachable from req.
	FinishWriteRequest(r)

	rowsInserted, err := ingestor.dispatcher.InsertData(model.Data{Rows: dataSamples, ReceivedTime: time.Now()})
	if err == nil && rowsInserted != totalRows {
		return rowsInserted, fmt.Errorf("failed to insert all the data! Expected: %d, Got: %d", totalRows, rowsInserted)
	}
	return rowsInserted, err
}

// Parts of metric creation not needed to insert data
func (ingestor *DBIngestor) CompleteMetricCreation() error {
	return ingestor.dispatcher.CompleteMetricCreation()
}

// Close closes the ingestor
func (ingestor *DBIngestor) Close() {
	ingestor.dispatcher.Close()
}
