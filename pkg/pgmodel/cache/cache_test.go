// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.
package cache

import (
	"testing"

	"github.com/timescale/promscale/pkg/clockcache"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
)

func TestMetricTableNameCache(t *testing.T) {
	testCases := []struct {
		name        string
		schema      string
		metric      string
		tableSchema string
		tableName   string
		seriesTable string
	}{
		{
			name:      "empty",
			metric:    "",
			tableName: "",
		},
		{
			name:      "simple metric",
			metric:    "metric",
			tableName: "metricTableName",
		},
		{
			name:      "metric as table name",
			metric:    "metric",
			tableName: "metric",
		},
		{
			name:      "empty table name",
			metric:    "metric",
			tableName: "",
		},
		{
			name:        "empty schema",
			metric:      "metric",
			tableName:   "tableName",
			seriesTable: "tableName",
		},
		{
			name:        "default schema",
			schema:      "",
			metric:      "metric",
			tableSchema: "schema",
			tableName:   "tableName",
			seriesTable: "tableName",
		},
		{
			name:        "with schema",
			schema:      "schema",
			metric:      "metric",
			tableSchema: "schema",
			tableName:   "tableName",
			seriesTable: "tableName",
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			cache := MetricNameCache{
				Metrics: clockcache.WithMax(100),
			}

			_, missing, _, err := cache.Get(c.schema, c.metric)

			if missing != "" {
				t.Fatal("found cache that should be missing, not stored yet")
			}

			if err != errors.ErrEntryNotFound {
				t.Fatalf("got unexpected error:\ngot\n%s\nwanted\n%s\n", err, errors.ErrEntryNotFound)
			}

			err = cache.Set(c.schema, c.metric, c.tableSchema, c.tableName, c.seriesTable)

			if err != nil {
				t.Fatalf("got unexpected error:\ngot\n%s\nwanted\nnil\n", err)
			}

			foundSchema, foundTableName, foundSeriesTable, err := cache.Get(c.schema, c.metric)

			if foundSchema != c.tableSchema {
				t.Fatalf("found wrong cache schema value: got %s wanted %s", foundSchema, c.schema)
			}
			if foundTableName != c.tableName {
				t.Fatalf("found wrong cache table name value: got %s wanted %s", foundTableName, c.tableName)
			}
			if foundSeriesTable != c.seriesTable {
				t.Fatalf("found wrong cache series table name value: got %s wanted %s", foundSeriesTable, c.seriesTable)
			}

			if err != nil {
				t.Fatalf("got unexpected error:\ngot\n%s\nwanted\nnil\n", err)
			}

			// Check if specific schema key is set with correct value
			if c.schema == "" && c.tableSchema != "" {
				foundSchema, foundTableName, foundSeriesTable, err = cache.Get(c.tableSchema, c.metric)

				if foundSchema != c.tableSchema {
					t.Fatalf("found wrong cache schema value: got %s wanted %s", foundSchema, c.tableSchema)
				}
				if foundTableName != c.tableName {
					t.Fatalf("found wrong cache table name value: got %s wanted %s", foundTableName, c.tableName)
				}
				if foundSeriesTable != c.seriesTable {
					t.Fatalf("found wrong cache series table name value: got %s wanted %s", foundSeriesTable, c.seriesTable)
				}

				if err != nil {
					t.Fatalf("got unexpected error:\ngot\n%s\nwanted\nnil\n", err)
				}
			}
		})
	}
}
