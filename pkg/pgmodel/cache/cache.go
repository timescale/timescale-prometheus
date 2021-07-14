// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package cache

import (
	"fmt"
	"strings"

	"github.com/timescale/promscale/pkg/clockcache"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
)

const (
	DefaultMetricCacheSize = 10000

	fieldSeparator = "*"
)

// MetricCache provides a caching mechanism for metric table names.
type MetricCache interface {
	Get(schema, metric string) (string, string, string, error)
	Set(schema, metric, tableSchema, tableName, seriesTable string) error
	// Len returns the number of metrics cached in the system.
	Len() int
	// Cap returns the capacity of the metrics cache.
	Cap() int
	Evictions() uint64
}

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

// MetricNameCache stores and retrieves metric table names in a in-memory cache.
type MetricNameCache struct {
	Metrics *clockcache.Cache
}

func (m *MetricNameCache) key(schema, metric string) string {
	// deep copy the strings so the original memory doesn't need to stick around
	keyBuilder := strings.Builder{}
	keyBuilder.Grow(len(metric) + len(schema) + len(fieldSeparator))
	keyBuilder.WriteString(schema)
	keyBuilder.WriteString(fieldSeparator)
	keyBuilder.WriteString(metric)
	return keyBuilder.String()
}

func (m *MetricNameCache) value(schema, tableName, seriesTable string) string {
	// deep copy the strings so the original memory doesn't need to stick around
	valueBuilder := strings.Builder{}
	valueBuilder.Grow(len(schema) + len(tableName) + len(seriesTable) + (len(fieldSeparator) * 2))
	valueBuilder.WriteString(schema)
	valueBuilder.WriteString(fieldSeparator)
	valueBuilder.WriteString(tableName)
	valueBuilder.WriteString(fieldSeparator)
	valueBuilder.WriteString(seriesTable)
	return valueBuilder.String()
}

func (m *MetricNameCache) fromValue(val string) (tableSchema, tableName, seriesTable string, err error) {
	values := strings.Split(val, fieldSeparator)

	if len(values) != 3 {
		return "", "", "", fmt.Errorf("invalid number of values stored")
	}

	return values[0], values[1], values[2], nil
}

// Get fetches the table name for specified metric.
func (m *MetricNameCache) Get(schema, metric string) (tableSchema, tableName, seriesTable string, err error) {
	key := m.key(schema, metric)
	result, ok := m.Metrics.Get(key)
	if !ok {
		return "", "", "", errors.ErrEntryNotFound
	}
	return m.fromValue(result.(string))
}

// Set stores metric info for specified metric with schema.
func (m *MetricNameCache) Set(schema, metric, tableSchema, tableName, seriesTable string) error {
	// deep copy the strings so the original memory doesn't need to stick around
	val := m.value(tableSchema, tableName, seriesTable)
	key := m.key(schema, metric)
	//size includes an 8-byte overhead for each string
	m.Metrics.Insert(key, val, uint64(len(key)+len(val)+16))

	// In case of empty schema, we should set the entry with specified schema.
	if schema == "" {
		key = m.key(tableSchema, metric)
		//size includes an 8-byte overhead for each string
		m.Metrics.Insert(key, val, uint64(len(key)+len(val)+16))
	}

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

func NewMetricCache(config Config) *MetricNameCache {
	return &MetricNameCache{Metrics: clockcache.WithMax(config.MetricsCacheSize)}
}

func NewLabelsCache(config Config) LabelsCache {
	return clockcache.WithMax(config.LabelsCacheSize)
}
