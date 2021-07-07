// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package ingestor

import (
	"context"
	"fmt"
	"strings"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/timescale/promscale/pkg/pgmodel/cache"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
	"github.com/timescale/promscale/pkg/pgmodel/common/schema"
	"github.com/timescale/promscale/pkg/pgmodel/model"
	"github.com/timescale/promscale/pkg/pgmodel/model/pgutf8str"
	"github.com/timescale/promscale/pkg/pgxconn"
)

const (
	seriesInsertSQL           = "SELECT (_prom_catalog.get_or_create_series_id_for_label_array($1, l.elem)).series_id, l.nr FROM unnest($2::prom_api.label_array[]) WITH ORDINALITY l(elem, nr) ORDER BY l.elem"
	createExemplarTable       = "SELECT * FROM " + schema.Catalog + ".create_exemplar_table_if_not_exists($1)"
	getExemplarLabelPositions = "SELECT * FROM " + schema.Catalog + ".get_exemplar_label_key_positions($1::TEXT, $2::TEXT[])"
)

type metricBatcher struct {
	conn            pgxconn.PgxConn
	input           chan *insertDataRequest
	pending         *pendingBuffer
	metricName      string
	metricTableName string
	toCopiers       chan copyRequest
	labelArrayOID   uint32
	exemplarCatalog *exemplarInfo
}

type exemplarInfo struct {
	seenPreviuosly bool
	exemplarCache  cache.PositionCache
}

// Create the metric table for the metric we handle, if it does not already
// exist. This only does the most critical part of metric table creation, the
// rest is handled by completeMetricTableCreation().
func initializeMetricBatcher(conn pgxconn.PgxConn, metricName string, completeMetricCreationSignal chan struct{}, metricTableNames cache.MetricCache) (tableName string, err error) {
	// Metric batchers are always initialized with metric names of samples and not of exemplars.
	var possiblyNew bool
	tableName, err = metricTableNames.Get(metricName, false)
	if err == errors.ErrEntryNotFound {
		tableName, possiblyNew, err = model.MetricTableName(conn, metricName)
		if err != nil {
			return "", fmt.Errorf("metric table name: %w", err)
		}

		// We ignore error here since this is just an optimization.
		//
		// Always set metric name while initializing with exemplars as false, since
		// the metric name set here is via fetching the metric name from metric table.
		//
		// Metric table is filled during start, but exemplar table is filled when we
		// first see an exemplar. Hence, that's the place to sent isExemplar as true.
		_ = metricTableNames.Set(metricName, tableName, false)

		if possiblyNew {
			// Pass a signal if there is space.
			select {
			case completeMetricCreationSignal <- struct{}{}:
			default:
			}
		}
	} else if err != nil {
		return "", fmt.Errorf("get metric name from metric table cache: %w", err)
	}
	return tableName, err
}

func runMetricBatcher(conn pgxconn.PgxConn,
	input chan *insertDataRequest,
	metricName string,
	completeMetricCreationSignal chan struct{},
	metricTableNames cache.MetricCache,
	exemplarKeyPos cache.PositionCache,
	toCopiers chan copyRequest,
	labelArrayOID uint32) {

	var (
		tableName   string
		firstReq    *insertDataRequest
		firstReqSet = false
	)
	for firstReq = range input {
		var err error
		tableName, err = initializeMetricBatcher(conn, metricName, completeMetricCreationSignal, metricTableNames)
		if err != nil {
			firstReq.reportResult(fmt.Errorf("initializing the insert routine has failed with %w", err))
		} else {
			firstReqSet = true
			break
		}
	}

	//input channel was closed before getting a successful request
	if !firstReqSet {
		return
	}

	handler := metricBatcher{
		conn:            conn,
		input:           input,
		pending:         NewPendingBuffer(),
		metricName:      metricName,
		metricTableName: tableName,
		toCopiers:       toCopiers,
		labelArrayOID:   labelArrayOID,
		exemplarCatalog: &exemplarInfo{
			exemplarCache: exemplarKeyPos,
		},
	}

	handler.handleReq(firstReq)

	// Grab new requests from our channel and handle them. We do this hot-load
	// style: we keep grabbing requests off the channel while we can do so
	// without blocking, and flush them to the next layer when we run out, or
	// reach a predetermined threshold. The theory is that wake/sleep and
	// flushing is relatively expensive, and can be easily amortized over
	// multiple requests, so it pays to batch as much as we are able. However,
	// writes to a given metric can be relatively rare, so if we don't have
	// additional requests immediately we're likely not going to for a while.
	for {
		if handler.pending.IsEmpty() {
			stillAlive := handler.blockingHandleReq()
			if !stillAlive {
				return
			}
			continue
		}

	hotReceive:
		for handler.nonblockingHandleReq() {
			if handler.pending.IsFull() {
				break hotReceive
			}
		}

		handler.flush()
	}
}

func (h *metricBatcher) blockingHandleReq() bool {
	req, ok := <-h.input
	if !ok {
		return false
	}

	h.handleReq(req)

	return true
}

func (h *metricBatcher) nonblockingHandleReq() bool {
	select {
	case req := <-h.input:
		h.handleReq(req)
		return true
	default:
		return false
	}
}

func (h *metricBatcher) handleReq(req *insertDataRequest) bool {
	h.pending.addReq(req)
	if h.pending.IsFull() {
		h.flushPending()
		return true
	}
	return false
}

func (h *metricBatcher) flush() {
	if h.pending.IsEmpty() {
		return
	}
	h.flushPending()
}

// Set all unset SeriesIds and flush to the next layer
func (h *metricBatcher) flushPending() {
	processErr := func(err error) {
		h.pending.reportResults(err)
		h.pending.release()
		h.pending = NewPendingBuffer()
	}
	containsExemplars, err := h.setSeriesIds(h.pending.batch.Data())
	if err != nil {
		processErr(err)
		return
	}
	if containsExemplars {
		err = h.processExemplars(h.pending.batch.Data())
		if err != nil {
			processErr(err)
			return
		}
	}

	MetricBatcherFlushSeries.Observe(float64(h.pending.batch.CountSeries()))
	h.toCopiers <- copyRequest{h.pending, h.metricTableName}
	h.pending = NewPendingBuffer()
}

func (h *metricBatcher) processExemplars(data []model.Insertable) error {
	if !h.exemplarCatalog.seenPreviuosly {
		// We are seeing the exemplar belonging to this metric first time. It may be the
		// first time of this exemplar in the database. So, let's attempt to create a table
		// if it does not exists.
		var created bool // Trivial. Should we remove the scan?
		err := h.conn.QueryRow(context.Background(), createExemplarTable, h.metricName).Scan(&created)
		if err != nil {
			return fmt.Errorf("checking exemplar table creation: %w", err)
		}
	}
	err := orderExemplarLabelValues(h.conn, h.exemplarCatalog, data)
	if err != nil {
		return fmt.Errorf("metric-batcher: ordering exemplar label values: %w", err)
	}
	return nil
}

func orderExemplarLabelValues(conn pgxconn.PgxConn, info *exemplarInfo, data []model.Insertable) error {
	var (
		batch                pgxconn.PgxBatch
		pendingIndexes       []int
		emptyLabelSetIndexes []bool // Exemplars can have empty label sets. Hence, skip result scan for such cases.
	)

	for i := range data {
		row := data[i]
		if row.Type() == model.Exemplar {
			labelKeyIndex, entryExists := info.exemplarCache.GetLabelPositions(row.GetSeries().MetricName())
			needsFetch := true
			if entryExists {
				if positionExists := row.OrderExemplarLabels(labelKeyIndex); positionExists {
					needsFetch = false
				}
			}
			if needsFetch {
				if batch == nil {
					// Allocate a batch only if required. If the cache does the job, why to waste on allocs.
					batch = conn.NewBatch()
				}
				batch.Queue(getExemplarLabelPositions, row.GetSeries().MetricName(), row.AllExemplarLabelKeys())
				pendingIndexes = append(pendingIndexes, i)
				emptyLabelSetIndexes = append(emptyLabelSetIndexes, len(row.AllExemplarLabelKeys()) == 0)
			}
		}
	}
	if len(pendingIndexes) > 0 {
		// There are positions that require to be fetched. Let's fetch them and fill our indexes.
		// pendingIndexes contain the exact array index for rows, where the cache miss were found. Let's
		// use the pendingIndexes to go to those rows and order the labels in exemplars quickly.
		results, err := conn.SendBatch(context.Background(), batch)
		if err != nil {
			return fmt.Errorf("sending fetch label key positions batch: %w", err)
		}
		defer results.Close()
		for i, index := range pendingIndexes {
			var (
				metricName      string
				labelKeyIndex   map[string]int
				isEmptyLabelSet = emptyLabelSetIndexes[i]
			)
			err := results.QueryRow().Scan(&metricName, &labelKeyIndex)
			if err != nil {
				if isEmptyLabelSet && strings.Contains(err.Error(), "no rows in result set") {
					// Labels set of exemplar empty. Hence, we do not expect any rows while scanning the output.
					continue
				}
				return fmt.Errorf("fetching label key positions: %w", err)
			}
			info.exemplarCache.SetorUpdateLabelPositions(metricName, labelKeyIndex)
			row := data[index]
			row.OrderExemplarLabels(labelKeyIndex) // We just filled the position, so no need to check if it exists or not.
		}
	}
	return nil
}

type labelInfo struct {
	labelID int32
	Pos     int32
}

// Set all seriesIds for a samplesInfo, fetching any missing ones from the DB,
// and repopulating the cache accordingly.
// returns: the tableName for the metric being inserted into
// TODO move up to the rest of insertHandler
func (h *metricBatcher) setSeriesIds(rows []model.Insertable) (containsExemplars bool, err error) {
	seriesToInsert := make([]*model.Series, 0, len(rows))
	containsExemplars = false
	for i, series := range rows {
		if !series.GetSeries().IsSeriesIDSet() {
			seriesToInsert = append(seriesToInsert, rows[i].GetSeries())
		}
		if series.Type() == model.Exemplar {
			containsExemplars = true
		}
	}
	if len(seriesToInsert) == 0 {
		return
	}

	metricName := seriesToInsert[0].MetricName()
	labelMap := make(map[labels.Label]labelInfo, len(seriesToInsert))
	labelList := model.NewLabelList(len(seriesToInsert))
	//logically should be a separate function but we want
	//to prevent labelMap from escaping, so keeping inline.
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
					if err := labelList.Add(names[i], values[i]); err != nil {
						return containsExemplars, fmt.Errorf("failed to add label to labelList: %w", err)
					}
				}
			}
		}
	}
	if len(labelMap) == 0 {
		return
	}

	//labels have to be created before series are since we need a canonical
	//ordering for label creation to avoid deadlocks. Otherwise, if we create
	//the labels for multiple series in same txn as we are creating the series,
	//the ordering of label creation can only be canonical within a series and
	//not across series.
	dbEpoch, maxPos, err := h.fillLabelIDs(metricName, labelList, labelMap)
	if err != nil {
		return containsExemplars, fmt.Errorf("error setting series ids: %w", err)
	}

	//create the label arrays
	labelArraySet, seriesToInsert, err := createLabelArrays(seriesToInsert, labelMap, maxPos)
	if err != nil {
		return containsExemplars, fmt.Errorf("error setting series ids: %w", err)
	}
	if len(labelArraySet) == 0 {
		return
	}

	labelArrayArray := model.GetCustomType(model.LabelArray)
	err = labelArrayArray.Set(labelArraySet)
	if err != nil {
		return containsExemplars, fmt.Errorf("error setting series id: cannot set label_array: %w", err)
	}
	res, err := h.conn.Query(context.Background(), seriesInsertSQL, metricName, labelArrayArray)
	if err != nil {
		return containsExemplars, fmt.Errorf("error setting series_id: cannot query for series_id: %w", err)
	}
	defer res.Close()

	count := 0
	for res.Next() {
		var (
			id         model.SeriesID
			ordinality int64
		)
		err := res.Scan(&id, &ordinality)
		if err != nil {
			return containsExemplars, fmt.Errorf("error setting series_id: cannot scan series_id: %w", err)
		}
		seriesToInsert[int(ordinality)-1].SetSeriesID(id, dbEpoch)
		count++
	}
	if err := res.Err(); err != nil {
		return containsExemplars, fmt.Errorf("error setting series_id: reading series id rows: %w", err)
	}
	if count != len(seriesToInsert) {
		//This should never happen according to the logic. This is purely defensive.
		//panic since we may have set the seriesID incorrectly above and may
		//get data corruption if we continue.
		panic(fmt.Sprintf("number series returned %d doesn't match expected series %d", count, len(seriesToInsert)))
	}
	return
}

func (h *metricBatcher) fillLabelIDs(metricName string, labelList *model.LabelList, labelMap map[labels.Label]labelInfo) (model.SeriesEpoch, int, error) {
	//we cannot use the label cache here because that maps label ids => name, value.
	//what we need here is name, value => id.
	//we may want a new cache for that, at a later time.

	batch := h.conn.NewBatch()
	var dbEpoch model.SeriesEpoch
	maxPos := 0

	names, values := labelList.Get()
	items := len(names.Elements)
	if items != len(labelMap) {
		return dbEpoch, 0, fmt.Errorf("error filling labels: number of items in labelList and labelMap doesn't match")
	}
	// The epoch will never decrease, so we can check it once at the beginning,
	// at worst we'll store too small an epoch, which is always safe
	batch.Queue("BEGIN;")
	batch.Queue(getEpochSQL)
	batch.Queue("COMMIT;")

	//getLabels in batches of 1000 to prevent locks on label creation
	//from being taken for too long.
	itemsPerBatch := 1000
	labelBatches := 0
	for i := 0; i < len(names.Elements); i += itemsPerBatch {
		labelBatches++
		high := i + itemsPerBatch
		if len(names.Elements) < high {
			high = len(names.Elements)
		}
		namesSlice, err := names.Slice(i, high)
		if err != nil {
			return dbEpoch, 0, fmt.Errorf("error filling labels: slicing names: %w", err)
		}
		valuesSlice, err := values.Slice(i, high)
		if err != nil {
			return dbEpoch, 0, fmt.Errorf("error filling labels: slicing values: %w", err)
		}
		batch.Queue("BEGIN;")
		batch.Queue("SELECT * FROM "+schema.Catalog+".get_or_create_label_ids($1, $2, $3)", metricName, namesSlice, valuesSlice)
		batch.Queue("COMMIT;")
	}
	br, err := h.conn.SendBatch(context.Background(), batch)
	if err != nil {
		return dbEpoch, 0, fmt.Errorf("error filling labels: %w", err)
	}
	defer br.Close()

	if _, err := br.Exec(); err != nil {
		return dbEpoch, 0, fmt.Errorf("error filling labels on begin: %w", err)
	}
	err = br.QueryRow().Scan(&dbEpoch)
	if err != nil {
		return dbEpoch, 0, fmt.Errorf("error filling labels: %w", err)
	}
	if _, err := br.Exec(); err != nil {
		return dbEpoch, 0, fmt.Errorf("error filling labels on commit: %w", err)
	}

	var count int
	for i := 0; i < labelBatches; i++ {
		if _, err := br.Exec(); err != nil {
			return dbEpoch, 0, fmt.Errorf("error filling labels on begin label batch: %w", err)
		}

		err := func() error {
			rows, err := br.Query()
			if err != nil {
				return fmt.Errorf("error filling labels: %w", err)
			}
			defer rows.Close()

			var (
				labelName  pgutf8str.Text
				labelValue pgutf8str.Text
			)

			for rows.Next() {
				res := labelInfo{}
				err := rows.Scan(&res.Pos, &res.labelID, &labelName, &labelValue)
				if err != nil {
					return fmt.Errorf("error filling labels in scan: %w", err)
				}
				key := labels.Label{Name: labelName.Get().(string), Value: labelValue.Get().(string)}
				_, ok := labelMap[key]
				if !ok {
					return fmt.Errorf("error filling labels: getting a key never sent to the db")
				}
				labelMap[key] = res
				if int(res.Pos) > maxPos {
					maxPos = int(res.Pos)
				}
				count++
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("error filling labels: error reading label id rows: %w", err)
			}
			return nil
		}()
		if err != nil {
			return dbEpoch, 0, err
		}
		if _, err := br.Exec(); err != nil {
			return dbEpoch, 0, fmt.Errorf("error filling labels on commit label batch: %w", err)
		}
	}
	if count != items {
		return dbEpoch, 0, fmt.Errorf("error filling labels: not filling as many items as expected: %v vs %v", count, items)
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
				return nil, nil, fmt.Errorf("error generating label array: missing key in map")
			}
			if res.labelID == 0 {
				return nil, nil, fmt.Errorf("error generating label array: missing id for label %v=>%v", names[i], values[i])
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
		return nil, nil, fmt.Errorf("error generating label array: lengths not equal")
	}
	return labelArraySet, series[:dest], nil
}
