package ingestor

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/timescale/promscale/pkg/pgmodel/cache"
	"github.com/timescale/promscale/pkg/pgmodel/model"
	"github.com/timescale/promscale/pkg/prompb"
)

func TestOrderExemplarLabelValues(t *testing.T) {
	rawExemplars := []prompb.Exemplar{
		{
			Labels:    []prompb.Label{{Name: "TraceID", Value: "some_trace_id"}, {Name: "component", Value: "tester"}},
			Value:     1.5,
			Timestamp: 1,
		},
		{
			Labels:    []prompb.Label{{Name: "app", Value: "test"}, {Name: "component", Value: "tester"}},
			Value:     2.5,
			Timestamp: 3,
		},
		{
			Labels:    []prompb.Label{}, // No labels. A valid label according to Open Metrics.
			Value:     3.5,
			Timestamp: 5,
		},
	}
	exemplarSeriesLabels := []prompb.Label{{Name: "__name__", Value: "exemplar_test_metric"}, {Name: "component", Value: "test_infra"}}

	series := model.NewSeries("hash_key", exemplarSeriesLabels)
	insertables := make([]model.Insertable, 3) // Since 3 exemplars.

	for i, exemplar := range rawExemplars {
		insertable := model.NewInsertable(series, []prompb.Exemplar{exemplar}) // To be in line with write request behaviour.
		insertables[i] = insertable
	}

	mockConn := model.NewSqlRecorder([]model.SqlQuery{}, t)
	posCache := cache.NewExemplarLabelsPosCache(cache.Config{ExemplarCacheSize: 4})
	prepareExemplarPosCache(posCache)

	exemplarCatalog := &exemplarInfo{
		exemplarCache: posCache,
	}
	err := orderExemplarLabelValues(mockConn, exemplarCatalog, insertables)
	require.NoError(t, err)

	// Verify exemplar label value positioning.
	require.Equal(t, []prompb.Label{{Value: "some_trace_id"}, {Value: "tester"}, {Value: model.EmptyExemplarValues}}, insertables[0].At(0).ExemplarLabels())
	require.Equal(t, []prompb.Label{{Value: model.EmptyExemplarValues}, {Value: "tester"}, {Value: "test"}}, insertables[1].At(0).ExemplarLabels())
	require.Equal(t, []prompb.Label{{Value: model.EmptyExemplarValues}, {Value: model.EmptyExemplarValues}, {Value: model.EmptyExemplarValues}}, insertables[2].At(0).ExemplarLabels())
	// Verify if all label names are empty.
	for _, i := range insertables {
		lbls := i.At(0).ExemplarLabels()
		for j := range lbls {
			require.Equal(t, "", lbls[j].Name, i)
		}
	}
}

func prepareExemplarPosCache(posCache cache.PositionCache) {
	index := map[string]int{
		"TraceID":   1,
		"component": 2,
		"app":       3,
	}
	posCache.SetorUpdateLabelPositions("exemplar_test_metric", index)
}
