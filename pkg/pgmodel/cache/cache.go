// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package cache

import (
	"strings"

	"github.com/timescale/promscale/pkg/clockcache"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
)

const (
	DefaultMetricCacheSize = 10000
)

type LabelsCache interface {
	// GetValues tries to get a batch of keys and store the corresponding values is valuesOut
	// returns the number of keys that were actually found.
	// NOTE: this function does _not_ preserve the order of keys; the first numFound
	//       keys will be the keys whose values are present, while the remainder
	//       will be the keys not present in the cache
	GetValues(keys []interface{}, valuesOut []interface{}) (numFound int)
	// InsertBatch inserts a batch of keys with their corresponding values.
	// This function will _overwrite_ the keys and values slices with their
	// canonical versions.
	// returns the number of elements inserted, is lower than len(keys) if insertion
	// starved
	InsertBatch(keys []interface{}, values []interface{}, sizes []uint64) (numInserted int)
	// Len returns the number of labels cached in the system.
	Len() int
	// Cap returns the capacity of the labels cache.
	Cap() int
	Evictions() uint64
}

// MetricCache provides a caching mechanism for metric table names.
type MetricCache interface {
	// Get returns the TableInfo if the given metric is present, otherwise
	// returns an errors.ErrEntryNotFound.
	Get(metric string, shouldBeExemplar bool) (string, error)
	// Set sets the table name corresponding to the given metric. It also
	// saves whether the incoming entry is for exemplar or not, as
	// ingested exemplars have a different table than ingested samples,
	// since ingestion of exemplars is independent of samples.
	Set(metric, tableName string, isExemplar bool) error
	// Len returns the number of metrics cached in the system.
	Len() int
	// Cap returns the capacity of the metrics cache.
	Cap() int
	Evictions() uint64
}

// todo: make this struct private only
// MetricNameCache stores and retrieves metric table names in a in-memory cache.
type MetricNameCache struct {
	Metrics *clockcache.Cache
}

func NewMetricCache(config Config) *MetricNameCache {
	return &MetricNameCache{Metrics: clockcache.WithMax(config.MetricsCacheSize)}
}

// Get fetches the table name for specified metric.
func (m *MetricNameCache) Get(metric string, shouldBeExemplar bool) (string, error) {
	result, ok := m.Metrics.Get(metricInfo{metric, shouldBeExemplar})
	if !ok {
		return "", errors.ErrEntryNotFound
	}
	return result.(string), nil
}

// metricInfo contains the metric name along with whether the (metric) entry is
// for exemplar or not.
type metricInfo struct {
	MetricName string
	IsExemplar bool
}

// Set stores table name for specified metric.
func (m *MetricNameCache) Set(metric string, tableName string, isExemplar bool) error {
	// deep copy the strings so the original memory doesn't need to stick around
	metricBuilder := strings.Builder{}
	metricBuilder.Grow(len(metric))
	metricBuilder.WriteString(metric)
	tableBuilder := strings.Builder{}
	tableBuilder.Grow(len(tableName))
	tableBuilder.WriteString(tableName)
	//size includes an 8-byte overhead for each string
	m.Metrics.Insert(metricInfo{metricBuilder.String(), isExemplar}, tableBuilder.String(), uint64(metricBuilder.Len()+tableBuilder.Len()+16))
	return nil
}

func (m *MetricNameCache) Len() int {
	return m.Metrics.Len()
}

func (m *MetricNameCache) Cap() int {
	return m.Metrics.Cap()
}

func (m *MetricNameCache) Evictions() uint64 {
	return m.Metrics.Evictions()
}

func NewLabelsCache(config Config) LabelsCache {
	return clockcache.WithMax(config.LabelsCacheSize)
}
