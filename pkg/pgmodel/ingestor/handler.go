// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package ingestor

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgtype"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/timescale/promscale/pkg/pgmodel/common/schema"
	"github.com/timescale/promscale/pkg/pgmodel/model"
	"github.com/timescale/promscale/pkg/pgxconn"
)

type insertHandler struct {
	conn            pgxconn.PgxConn
	input           chan *insertDataRequest
	pending         *pendingBuffer
	metricTableName string
	toCopiers       chan copyRequest
	labelArrayOID   uint32
}

func (h *insertHandler) blockingHandleReq() bool {
	req, ok := <-h.input
	if !ok {
		return false
	}

	h.handleReq(req)

	return true
}

func (h *insertHandler) nonblockingHandleReq() bool {
	select {
	case req := <-h.input:
		h.handleReq(req)
		return true
	default:
		return false
	}
}

func (h *insertHandler) handleReq(req *insertDataRequest) bool {
	h.pending.addReq(req)
	if h.pending.IsFull() {
		h.flushPending()
		return true
	}
	return false
}

func (h *insertHandler) flush() {
	if h.pending.IsEmpty() {
		return
	}
	h.flushPending()
}

// Set all unset SeriesIds and flush to the next layer
func (h *insertHandler) flushPending() {
	err := h.setSeriesIds(h.pending.batch.GetSeriesSamples())
	if err != nil {
		h.pending.reportResults(err)
		h.pending.release()
		h.pending = NewPendingBuffer()
		return
	}

	h.toCopiers <- copyRequest{h.pending, h.metricTableName}
	h.pending = NewPendingBuffer()
}

type labelInfo struct {
	labelID int32
	Pos     int32
}

// Set all seriesIds for a samplesInfo, fetching any missing ones from the DB,
// and repopulating the cache accordingly.
// returns: the tableName for the metric being inserted into
// TODO move up to the rest of insertHandler
func (h *insertHandler) setSeriesIds(seriesSamples []model.Samples) error {
	seriesToInsert := make([]*model.Series, 0, len(seriesSamples))
	for i, series := range seriesSamples {
		if !series.GetSeries().IsSeriesIDSet() {
			seriesToInsert = append(seriesToInsert, seriesSamples[i].GetSeries())
		}
	}
	if len(seriesToInsert) == 0 {
		return nil
	}

	metricName := seriesToInsert[0].MetricName()
	labelMap := make(map[labels.Label]labelInfo, len(seriesToInsert))
	labelList := model.NewLabelList(len(seriesToInsert))
	{
		for _, series := range seriesToInsert {
			names, values, ok := series.NameValues()
			if !ok {
				//was already set
				continue
			}
			for i := range names {
				key := labels.Label{Name: names[i], Value: values[i]}
				_, ok = labelMap[key]
				if !ok {
					labelMap[key] = labelInfo{}
					labelList.Add(names[i], values[i])
				}
			}
		}
		sort.Sort(labelList)
	}
	if len(labelMap) == 0 {
		return nil
	}

	//labels have to be created before series are since we need a canonical
	//ordering for label creation to avoid deadlocks. Otherwise, if we create
	//the labels for multiple series in same txn as we are creating the series,
	//the ordering of label creation can only be canonical within a series and
	//not across series.
	dbEpoch, maxPos, err := h.fillLabelIDs(metricName, labelList, labelMap)
	if err != nil {
		return err
	}

	//create the label arrays
	labelArraySet, seriesToInsert, err := createLabelArrays(seriesToInsert, labelMap, maxPos)
	if err != nil {
		return fmt.Errorf("Error setting series ids: %w", err)
	}
	if len(labelArraySet) == 0 {
		return nil
	}

	labelArrayArray := pgtype.NewArrayType("prom_api.label_array[]", h.labelArrayOID, func() pgtype.ValueTranscoder { return &pgtype.Int4Array{} })
	err = labelArrayArray.Set(labelArraySet)
	if err != nil {
		return fmt.Errorf("Error setting series id: cannot set label_array: %w", err)
	}
	res, err := h.conn.Query(context.Background(), "SELECT r.series_id, l.nr FROM unnest($2::prom_api.label_array[]) WITH ORDINALITY l(elem, nr) INNER JOIN LATERAL _prom_catalog.get_or_create_series_id_for_label_array($1, l.elem) r ON (TRUE)", metricName, labelArrayArray)
	if err != nil {
		panic(err)
	}
	defer res.Close()

	for res.Next() {
		var id model.SeriesID
		var ordinality int64
		err := res.Scan(&id, &ordinality)
		if err != nil {
			panic(err)
		}
		seriesToInsert[int(ordinality)-1].SetSeriesID(id, dbEpoch)
	}
	return nil
}

func (h *insertHandler) fillLabelIDs(metricName string, labelList *model.LabelList, labelMap map[labels.Label]labelInfo) (model.SeriesEpoch, int, error) {
	//we cannot use the label cache here because that maps label ids => name, value.
	//what we need here is name, value => id.
	//we may want a new cache for that, at a later time.

	batch := h.conn.NewBatch()
	var dbEpoch model.SeriesEpoch
	maxPos := 0

	// The epoch will never decrease, so we can check it once at the beginning,
	// at worst we'll store too small an epoch, which is always safe
	batch.Queue(getEpochSQL)
	batch.Queue("SELECT * FROM "+schema.Catalog+".get_or_create_label_ids($1, $2, $3)", metricName, labelList.Names, labelList.Values)
	br, err := h.conn.SendBatch(context.Background(), batch)
	if err != nil {
		return dbEpoch, 0, fmt.Errorf("Error filling labels: %w", err)
	}
	defer br.Close()

	err = br.QueryRow().Scan(&dbEpoch)
	if err != nil {
		return dbEpoch, 0, fmt.Errorf("Error filling labels: %w", err)
	}
	rows, err := br.Query()
	if err != nil {
		return dbEpoch, 0, fmt.Errorf("Error filling labels: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		key := labels.Label{}
		res := labelInfo{}
		err := rows.Scan(&res.Pos, &res.labelID, &key.Name, &key.Value)
		if err != nil {
			return dbEpoch, 0, fmt.Errorf("Error filling labels: %w", err)
		}
		_, ok := labelMap[key]
		if !ok {
			return dbEpoch, 0, fmt.Errorf("Error filling labels: getting a key never sent to the db")
		}
		labelMap[key] = res
		if int(res.Pos) > maxPos {
			maxPos = int(res.Pos)
		}
	}
	return dbEpoch, maxPos, nil
}

func createLabelArrays(series []*model.Series, labelMap map[labels.Label]labelInfo, maxPos int) ([][]int32, []*model.Series, error) {
	labelArraySet := make([][]int32, 0, len(series))
	dest := 0
	for src := 0; src < len(series); src++ {
		names, values, ok := series[src].NameValues()
		if !ok {
			continue
		}
		lArray := make([]int32, maxPos)
		maxIndex := 0
		for i := range names {
			key := labels.Label{Name: names[i], Value: values[i]}
			res, ok := labelMap[key]
			if !ok {
				return nil, nil, fmt.Errorf("Error generating label array: missing key in map")
			}
			if res.labelID == 0 {
				return nil, nil, fmt.Errorf("Error generating label array: missing id for label")
			}
			//Pos is 1-indexed, slices are 0-indexed
			sliceIndex := int(res.Pos) - 1
			lArray[sliceIndex] = int32(res.labelID)
			if sliceIndex > maxIndex {
				maxIndex = sliceIndex
			}
		}
		lArray = lArray[:maxIndex+1]
		labelArraySet = append(labelArraySet, lArray)
		//this logic is needed for when continue is hit above
		if src != dest {
			series[dest] = series[src]
		}
		dest++
	}
	if len(labelArraySet) != len(series[:dest]) {
		return nil, nil, fmt.Errorf("Error generating label array: lengths not equal")
	}
	return labelArraySet, series[:dest], nil
}
