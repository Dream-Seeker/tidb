// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mocktikv

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/juju/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tipb/go-tipb"
	goctx "golang.org/x/net/context"
)

type executor interface {
	SetSrcExec(executor)
	GetSrcExec() executor
	Count() int64
	Next(goCtx goctx.Context) ([][]byte, error)
}

type tableScanExec struct {
	*tipb.TableScan
	colIDs         map[int64]int
	kvRanges       []kv.KeyRange
	startTS        uint64
	isolationLevel kvrpcpb.IsolationLevel
	mvccStore      MVCCStore
	cursor         int
	seekKey        []byte
	count          int64

	src executor
}

func (e *tableScanExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *tableScanExec) GetSrcExec() executor {
	return e.src
}

func (e *tableScanExec) Count() int64 {
	return e.count
}

func (e *tableScanExec) Next(goCtx goctx.Context) (value [][]byte, err error) {
	e.count++
	for e.cursor < len(e.kvRanges) {
		ran := e.kvRanges[e.cursor]
		if ran.IsPoint() {
			value, err = e.getRowFromPoint(ran)
			if err != nil {
				return nil, errors.Trace(err)
			}
			e.cursor++
			if value == nil {
				continue
			}
			return value, nil
		}

		value, err = e.getRowFromRange(ran)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if value == nil {
			e.seekKey = nil
			e.cursor++
			continue
		}
		return value, nil
	}

	return nil, nil
}

func (e *tableScanExec) getRowFromPoint(ran kv.KeyRange) ([][]byte, error) {
	val, err := e.mvccStore.Get(ran.StartKey, e.startTS, e.isolationLevel)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(val) == 0 {
		return nil, nil
	}
	handle, err := tablecodec.DecodeRowKey(ran.StartKey)
	if err != nil {
		return nil, errors.Trace(err)
	}
	row, err := getRowData(e.Columns, e.colIDs, handle, val)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return row, nil
}

func (e *tableScanExec) getRowFromRange(ran kv.KeyRange) ([][]byte, error) {
	if e.seekKey == nil {
		if e.Desc {
			e.seekKey = ran.EndKey
		} else {
			e.seekKey = ran.StartKey
		}
	}
	var pairs []Pair
	var pair Pair
	if e.Desc {
		pairs = e.mvccStore.ReverseScan(ran.StartKey, e.seekKey, 1, e.startTS, e.isolationLevel)
	} else {
		pairs = e.mvccStore.Scan(e.seekKey, ran.EndKey, 1, e.startTS, e.isolationLevel)
	}
	if len(pairs) > 0 {
		pair = pairs[0]
	}
	if pair.Err != nil {
		// TODO: Handle lock error.
		return nil, errors.Trace(pair.Err)
	}
	if pair.Key == nil {
		return nil, nil
	}
	if e.Desc {
		if bytes.Compare(pair.Key, ran.StartKey) < 0 {
			return nil, nil
		}
		e.seekKey = []byte(tablecodec.TruncateToRowKeyLen(kv.Key(pair.Key)))
	} else {
		if bytes.Compare(pair.Key, ran.EndKey) >= 0 {
			return nil, nil
		}
		e.seekKey = []byte(kv.Key(pair.Key).PrefixNext())
	}

	handle, err := tablecodec.DecodeRowKey(pair.Key)
	if err != nil {
		return nil, errors.Trace(err)
	}
	row, err := getRowData(e.Columns, e.colIDs, handle, pair.Value)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return row, nil
}

const (
	pkColNotExists = iota
	pkColIsSigned
	pkColIsUnsigned
)

type indexScanExec struct {
	*tipb.IndexScan
	colsLen        int
	kvRanges       []kv.KeyRange
	startTS        uint64
	isolationLevel kvrpcpb.IsolationLevel
	mvccStore      MVCCStore
	cursor         int
	seekKey        []byte
	pkStatus       int
	count          int64

	src executor
}

func (e *indexScanExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *indexScanExec) GetSrcExec() executor {
	return e.src
}

func (e *indexScanExec) Count() int64 {
	return e.count
}

func (e *indexScanExec) isUnique() bool {
	return e.Unique != nil && *e.Unique
}

func (e *indexScanExec) Next(goCtx goctx.Context) (value [][]byte, err error) {
	e.count++
	for e.cursor < len(e.kvRanges) {
		ran := e.kvRanges[e.cursor]
		if ran.IsPoint() && e.isUnique() {
			value, err = e.getRowFromPoint(ran)
			if err != nil {
				return nil, errors.Trace(err)
			}
			e.cursor++
			if value == nil {
				continue
			}
		} else {
			value, err = e.getRowFromRange(ran)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if value == nil {
				e.cursor++
				e.seekKey = nil
				continue
			}
		}
		return value, nil
	}

	return nil, nil
}

// getRowFromPoint is only used for unique key.
func (e *indexScanExec) getRowFromPoint(ran kv.KeyRange) ([][]byte, error) {
	val, err := e.mvccStore.Get(ran.StartKey, e.startTS, e.isolationLevel)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(val) == 0 {
		return nil, nil
	}
	return e.decodeIndexKV(Pair{Key: ran.StartKey, Value: val})
}

func (e *indexScanExec) decodeIndexKV(pair Pair) ([][]byte, error) {
	values, b, err := tablecodec.CutIndexKeyNew(pair.Key, e.colsLen)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(b) > 0 {
		if e.pkStatus != pkColNotExists {
			values = append(values, b)
		}
	} else if e.pkStatus != pkColNotExists {
		handle, err := decodeHandle(pair.Value)
		if err != nil {
			return nil, errors.Trace(err)
		}
		var handleDatum types.Datum
		if e.pkStatus == pkColIsUnsigned {
			handleDatum = types.NewUintDatum(uint64(handle))
		} else {
			handleDatum = types.NewIntDatum(handle)
		}
		handleBytes, err := codec.EncodeValue(b, handleDatum)
		if err != nil {
			return nil, errors.Trace(err)
		}
		values = append(values, handleBytes)
	}

	return values, nil
}

func (e *indexScanExec) getRowFromRange(ran kv.KeyRange) ([][]byte, error) {
	if e.seekKey == nil {
		if e.Desc {
			e.seekKey = ran.EndKey
		} else {
			e.seekKey = ran.StartKey
		}
	}
	var pairs []Pair
	var pair Pair
	if e.Desc {
		pairs = e.mvccStore.ReverseScan(ran.StartKey, e.seekKey, 1, e.startTS, e.isolationLevel)
	} else {
		pairs = e.mvccStore.Scan(e.seekKey, ran.EndKey, 1, e.startTS, e.isolationLevel)
	}
	if len(pairs) > 0 {
		pair = pairs[0]
	}
	if pair.Err != nil {
		// TODO: Handle lock error.
		return nil, errors.Trace(pair.Err)
	}
	if pair.Key == nil {
		return nil, nil
	}
	if e.Desc {
		if bytes.Compare(pair.Key, ran.StartKey) < 0 {
			return nil, nil
		}
		e.seekKey = pair.Key
	} else {
		if bytes.Compare(pair.Key, ran.EndKey) >= 0 {
			return nil, nil
		}
		e.seekKey = []byte(kv.Key(pair.Key).PrefixNext())
	}

	return e.decodeIndexKV(pair)
}

type selectionExec struct {
	conditions        []expression.Expression
	relatedColOffsets []int
	row               []types.Datum
	evalCtx           *evalContext
	count             int64
	src               executor
}

func (e *selectionExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *selectionExec) GetSrcExec() executor {
	return e.src
}

func (e *selectionExec) Count() int64 {
	return e.count
}

// evalBool evaluates expression to a boolean value.
func evalBool(exprs []expression.Expression, row types.DatumRow, ctx *stmtctx.StatementContext) (bool, error) {
	for _, expr := range exprs {
		data, err := expr.Eval(row)
		if err != nil {
			return false, errors.Trace(err)
		}
		if data.IsNull() {
			return false, nil
		}

		isBool, err := data.ToBool(ctx)
		if err != nil {
			return false, errors.Trace(err)
		}
		if isBool == 0 {
			return false, nil
		}
	}
	return true, nil
}

func (e *selectionExec) Next(goCtx goctx.Context) (value [][]byte, err error) {
	e.count++
	for {
		value, err = e.src.Next(goCtx)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if value == nil {
			return nil, nil
		}

		err = e.evalCtx.decodeRelatedColumnVals(e.relatedColOffsets, value, e.row)
		if err != nil {
			return nil, errors.Trace(err)
		}
		match, err := evalBool(e.conditions, e.row, e.evalCtx.sc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if match {
			return value, nil
		}
	}
}

type aggCtxsMapper map[string][]*aggregation.AggEvaluateContext

type aggregateExec struct {
	evalCtx           *evalContext
	aggExprs          []aggregation.Aggregation
	aggCtxsMap        aggCtxsMapper
	groupByExprs      []expression.Expression
	relatedColOffsets []int
	row               types.DatumRow
	groups            map[string]struct{}
	groupKeys         [][]byte
	groupKeyRows      [][][]byte
	executed          bool
	currGroupIdx      int
	count             int64

	src executor
}

func (e *aggregateExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *aggregateExec) GetSrcExec() executor {
	return e.src
}

func (e *aggregateExec) Count() int64 {
	return e.count
}

func (e *aggregateExec) innerNext(goCtx goctx.Context) (bool, error) {
	values, err := e.src.Next(goCtx)
	if err != nil {
		return false, errors.Trace(err)
	}
	if values == nil {
		return false, nil
	}
	err = e.aggregate(values)
	if err != nil {
		return false, errors.Trace(err)
	}
	return true, nil
}

func (e *aggregateExec) Next(goCtx goctx.Context) (value [][]byte, err error) {
	e.count++
	if e.aggCtxsMap == nil {
		e.aggCtxsMap = make(aggCtxsMapper, 0)
	}
	if !e.executed {
		for {
			hasMore, err := e.innerNext(goCtx)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if !hasMore {
				break
			}
		}
		e.executed = true
	}

	if e.currGroupIdx >= len(e.groups) {
		return nil, nil
	}
	gk := e.groupKeys[e.currGroupIdx]
	value = make([][]byte, 0, len(e.groupByExprs)+2*len(e.aggExprs))
	aggCtxs := e.getContexts(gk)
	for i, agg := range e.aggExprs {
		partialResults := agg.GetPartialResult(aggCtxs[i])
		for _, result := range partialResults {
			data, err := codec.EncodeValue(nil, result)
			if err != nil {
				return nil, errors.Trace(err)
			}
			value = append(value, data)
		}
	}
	value = append(value, e.groupKeyRows[e.currGroupIdx]...)
	e.currGroupIdx++

	return value, nil
}

func (e *aggregateExec) getGroupKey() ([]byte, [][]byte, error) {
	length := len(e.groupByExprs)
	if length == 0 {
		return nil, nil, nil
	}
	bufLen := 0
	row := make([][]byte, 0, length)
	for _, item := range e.groupByExprs {
		v, err := item.Eval(e.row)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		b, err := codec.EncodeValue(nil, v)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		bufLen += len(b)
		row = append(row, b)
	}
	buf := make([]byte, 0, bufLen)
	for _, col := range row {
		buf = append(buf, col...)
	}
	return buf, row, nil
}

// aggregate updates aggregate functions with row.
func (e *aggregateExec) aggregate(value [][]byte) error {
	err := e.evalCtx.decodeRelatedColumnVals(e.relatedColOffsets, value, e.row)
	if err != nil {
		return errors.Trace(err)
	}
	// Get group key.
	gk, gbyKeyRow, err := e.getGroupKey()
	if err != nil {
		return errors.Trace(err)
	}
	if _, ok := e.groups[string(gk)]; !ok {
		e.groups[string(gk)] = struct{}{}
		e.groupKeys = append(e.groupKeys, gk)
		e.groupKeyRows = append(e.groupKeyRows, gbyKeyRow)
	}
	// Update aggregate expressions.
	aggCtxs := e.getContexts(gk)
	for i, agg := range e.aggExprs {
		err = agg.Update(aggCtxs[i], e.evalCtx.sc, e.row)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (e *aggregateExec) getContexts(groupKey []byte) []*aggregation.AggEvaluateContext {
	groupKeyString := string(groupKey)
	aggCtxs, ok := e.aggCtxsMap[groupKeyString]
	if !ok {
		aggCtxs = make([]*aggregation.AggEvaluateContext, 0, len(e.aggExprs))
		for _, agg := range e.aggExprs {
			aggCtxs = append(aggCtxs, agg.CreateContext())
		}
		e.aggCtxsMap[groupKeyString] = aggCtxs
	}
	return aggCtxs
}

type topNExec struct {
	heap              *topNHeap
	evalCtx           *evalContext
	relatedColOffsets []int
	orderByExprs      []expression.Expression
	row               types.DatumRow
	cursor            int
	executed          bool
	count             int64

	src executor
}

func (e *topNExec) SetSrcExec(src executor) {
	e.src = src
}

func (e *topNExec) GetSrcExec() executor {
	return e.src
}

func (e *topNExec) Count() int64 {
	return e.count
}

func (e *topNExec) innerNext(goCtx goctx.Context) (bool, error) {
	value, err := e.src.Next(goCtx)
	if err != nil {
		return false, errors.Trace(err)
	}
	if value == nil {
		return false, nil
	}
	err = e.evalTopN(value)
	if err != nil {
		return false, errors.Trace(err)
	}
	return true, nil
}

func (e *topNExec) Next(goCtx goctx.Context) (value [][]byte, err error) {
	e.count++
	if !e.executed {
		for {
			hasMore, err := e.innerNext(goCtx)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if !hasMore {
				break
			}
		}
		e.executed = true
	}
	if e.cursor >= len(e.heap.rows) {
		return nil, nil
	}
	sort.Sort(&e.heap.topNSorter)
	row := e.heap.rows[e.cursor]
	e.cursor++

	return row.data, nil
}

// evalTopN evaluates the top n elements from the data. The input receives a record including its handle and data.
// And this function will check if this record can replace one of the old records.
func (e *topNExec) evalTopN(value [][]byte) error {
	newRow := &sortRow{
		key: make([]types.Datum, len(value)),
	}
	err := e.evalCtx.decodeRelatedColumnVals(e.relatedColOffsets, value, e.row)
	if err != nil {
		return errors.Trace(err)
	}
	for i, expr := range e.orderByExprs {
		newRow.key[i], err = expr.Eval(e.row)
		if err != nil {
			return errors.Trace(err)
		}
	}

	if e.heap.tryToAddRow(newRow) {
		for _, val := range value {
			newRow.data = append(newRow.data, val)
		}
	}
	return errors.Trace(e.heap.err)
}

type limitExec struct {
	limit  uint64
	cursor uint64
	count  int64

	src executor
}

func (e *limitExec) SetSrcExec(src executor) {
	e.src = src
}

func (e *limitExec) GetSrcExec() executor {
	return e.src
}

func (e *limitExec) Count() int64 {
	return e.count
}

func (e *limitExec) Next(goCtx goctx.Context) (value [][]byte, err error) {
	e.count++
	if e.cursor >= e.limit {
		return nil, nil
	}

	value, err = e.src.Next(goCtx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if value == nil {
		return nil, nil
	}
	e.cursor++
	return value, nil
}

func hasColVal(data [][]byte, colIDs map[int64]int, id int64) bool {
	offset, ok := colIDs[id]
	if ok && data[offset] != nil {
		return true
	}
	return false
}

// getRowData decodes raw byte slice to row data.
func getRowData(columns []*tipb.ColumnInfo, colIDs map[int64]int, handle int64, value []byte) ([][]byte, error) {
	values, err := tablecodec.CutRowNew(value, colIDs)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if values == nil {
		values = make([][]byte, len(colIDs))
	}
	// Fill the handle and null columns.
	for _, col := range columns {
		id := col.GetColumnId()
		offset := colIDs[id]
		if col.GetPkHandle() || id == model.ExtraHandleID {
			var handleDatum types.Datum
			if mysql.HasUnsignedFlag(uint(col.GetFlag())) {
				// PK column is Unsigned.
				handleDatum = types.NewUintDatum(uint64(handle))
			} else {
				handleDatum = types.NewIntDatum(handle)
			}
			handleData, err1 := codec.EncodeValue(nil, handleDatum)
			if err1 != nil {
				return nil, errors.Trace(err1)
			}
			values[offset] = handleData
			continue
		}
		if hasColVal(values, colIDs, id) {
			continue
		}
		if len(col.DefaultVal) > 0 {
			values[offset] = col.DefaultVal
			continue
		}
		if mysql.HasNotNullFlag(uint(col.GetFlag())) {
			return nil, errors.Errorf("Miss column %d", id)
		}

		values[offset] = []byte{codec.NilFlag}
	}

	return values, nil
}

func convertToExprs(sc *stmtctx.StatementContext, fieldTps []*types.FieldType, pbExprs []*tipb.Expr) ([]expression.Expression, error) {
	exprs := make([]expression.Expression, 0, len(pbExprs))
	for _, expr := range pbExprs {
		e, err := expression.PBToExpr(expr, fieldTps, sc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		exprs = append(exprs, e)
	}
	return exprs, nil
}

func decodeHandle(data []byte) (int64, error) {
	var h int64
	buf := bytes.NewBuffer(data)
	err := binary.Read(buf, binary.BigEndian, &h)
	return h, errors.Trace(err)
}
