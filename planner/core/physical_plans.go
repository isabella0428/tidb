// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"fmt"
	"strconv"
	"unsafe"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/planner/property"
	"github.com/pingcap/tidb/planner/util"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/plancodec"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tidb/util/size"
	"github.com/pingcap/tidb/util/stringutil"
	"github.com/pingcap/tidb/util/tracing"
	"github.com/pingcap/tipb/go-tipb"
)

var (
	_ PhysicalPlan = &PhysicalSelection{}
	_ PhysicalPlan = &PhysicalProjection{}
	_ PhysicalPlan = &PhysicalTopN{}
	_ PhysicalPlan = &PhysicalMaxOneRow{}
	_ PhysicalPlan = &PhysicalTableDual{}
	_ PhysicalPlan = &PhysicalUnionAll{}
	_ PhysicalPlan = &PhysicalSort{}
	_ PhysicalPlan = &NominalSort{}
	_ PhysicalPlan = &PhysicalLock{}
	_ PhysicalPlan = &PhysicalLimit{}
	_ PhysicalPlan = &PhysicalIndexScan{}
	_ PhysicalPlan = &PhysicalTableScan{}
	_ PhysicalPlan = &PhysicalTableReader{}
	_ PhysicalPlan = &PhysicalIndexReader{}
	_ PhysicalPlan = &PhysicalIndexLookUpReader{}
	_ PhysicalPlan = &PhysicalIndexMergeReader{}
	_ PhysicalPlan = &PhysicalHashAgg{}
	_ PhysicalPlan = &PhysicalStreamAgg{}
	_ PhysicalPlan = &PhysicalApply{}
	_ PhysicalPlan = &PhysicalIndexJoin{}
	_ PhysicalPlan = &PhysicalHashJoin{}
	_ PhysicalPlan = &PhysicalMergeJoin{}
	_ PhysicalPlan = &PhysicalUnionScan{}
	_ PhysicalPlan = &PhysicalWindow{}
	_ PhysicalPlan = &PhysicalShuffle{}
	_ PhysicalPlan = &PhysicalShuffleReceiverStub{}
	_ PhysicalPlan = &BatchPointGetPlan{}
	_ PhysicalPlan = &PhysicalTableSample{}
)

type tableScanAndPartitionInfo struct {
	tableScan     *PhysicalTableScan
	partitionInfo PartitionInfo
}

// MemoryUsage return the memory usage of tableScanAndPartitionInfo
func (t *tableScanAndPartitionInfo) MemoryUsage() (sum int64) {
	if t == nil {
		return
	}

	sum += t.partitionInfo.MemoryUsage()
	if t.tableScan != nil {
		sum += t.tableScan.MemoryUsage()
	}
	return
}

// ReadReqType is the read request type of the operator. Currently, only PhysicalTableReader uses this.
type ReadReqType uint8

const (
	// Cop means read from storage by cop request.
	Cop ReadReqType = iota
	// BatchCop means read from storage by BatchCop request, only used for TiFlash
	BatchCop
	// MPP means read from storage by MPP request, only used for TiFlash
	MPP
)

// Name returns the name of read request type.
func (r ReadReqType) Name() string {
	switch r {
	case BatchCop:
		return "batchCop"
	case MPP:
		return "mpp"
	default:
		// return cop by default
		return "cop"
	}
}

// PhysicalTableReader is the table reader in tidb.
type PhysicalTableReader struct {
	physicalSchemaProducer

	// TablePlans flats the tablePlan to construct executor pb.
	TablePlans []PhysicalPlan
	tablePlan  PhysicalPlan

	// StoreType indicates table read from which type of store.
	StoreType kv.StoreType

	// ReadReqType is the read request type for current physical table reader, there are 3 kinds of read request: Cop,
	// BatchCop and MPP, currently, the latter two are only used in TiFlash
	ReadReqType ReadReqType

	IsCommonHandle bool

	// Used by partition table.
	PartitionInfo PartitionInfo
	// Used by MPP, because MPP plan may contain join/union/union all, it is possible that a physical table reader contains more than 1 table scan
	PartitionInfos []tableScanAndPartitionInfo
}

// PartitionInfo indicates partition helper info in physical plan.
type PartitionInfo struct {
	PruningConds   []expression.Expression
	PartitionNames []model.CIStr
	Columns        []*expression.Column
	ColumnNames    types.NameSlice
}

const emptyPartitionInfoSize = int64(unsafe.Sizeof(PartitionInfo{}))

// MemoryUsage return the memory usage of PartitionInfo
func (pi *PartitionInfo) MemoryUsage() (sum int64) {
	if pi == nil {
		return
	}

	sum = emptyPartitionInfoSize
	for _, cond := range pi.PruningConds {
		sum += cond.MemoryUsage()
	}
	for _, cis := range pi.PartitionNames {
		sum += cis.MemoryUsage()
	}
	for _, col := range pi.Columns {
		sum += col.MemoryUsage()
	}
	for _, colName := range pi.ColumnNames {
		sum += colName.MemoryUsage()
	}
	return
}

// GetTablePlan exports the tablePlan.
func (p *PhysicalTableReader) GetTablePlan() PhysicalPlan {
	return p.tablePlan
}

// GetTableScans exports the tableScan that contained in tablePlans.
func (p *PhysicalTableReader) GetTableScans() []*PhysicalTableScan {
	tableScans := make([]*PhysicalTableScan, 0, 1)
	for _, tablePlan := range p.TablePlans {
		tableScan, ok := tablePlan.(*PhysicalTableScan)
		if ok {
			tableScans = append(tableScans, tableScan)
		}
	}
	return tableScans
}

// GetTableScan exports the tableScan that contained in tablePlans and return error when the count of table scan != 1.
func (p *PhysicalTableReader) GetTableScan() (*PhysicalTableScan, error) {
	tableScans := p.GetTableScans()
	if len(tableScans) != 1 {
		return nil, errors.New("the count of table scan != 1")
	}
	return tableScans[0], nil
}

// GetAvgRowSize return the average row size of this plan.
func (p *PhysicalTableReader) GetAvgRowSize() float64 {
	return getTblStats(p.tablePlan).GetAvgRowSize(p.ctx, p.tablePlan.Schema().Columns, false, false)
}

// MemoryUsage return the memory usage of PhysicalTableReader
func (p *PhysicalTableReader) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfUint8*2 + size.SizeOfBool + p.PartitionInfo.MemoryUsage()
	if p.tablePlan != nil {
		sum += p.tablePlan.MemoryUsage()
	}
	// since TablePlans is the flats of tablePlan, so we don't count it
	for _, pInfo := range p.PartitionInfos {
		sum += pInfo.MemoryUsage()
	}
	return
}

// setMppOrBatchCopForTableScan set IsMPPOrBatchCop for all TableScan.
func setMppOrBatchCopForTableScan(curPlan PhysicalPlan) {
	if ts, ok := curPlan.(*PhysicalTableScan); ok {
		ts.IsMPPOrBatchCop = true
	}
	children := curPlan.Children()
	for _, child := range children {
		setMppOrBatchCopForTableScan(child)
	}
}

// GetPhysicalTableReader returns PhysicalTableReader for logical TiKVSingleGather.
func (sg *TiKVSingleGather) GetPhysicalTableReader(schema *expression.Schema, stats *property.StatsInfo, props ...*property.PhysicalProperty) *PhysicalTableReader {
	reader := PhysicalTableReader{}.Init(sg.ctx, sg.blockOffset)
	reader.PartitionInfo = PartitionInfo{
		PruningConds:   sg.Source.allConds,
		PartitionNames: sg.Source.partitionNames,
		Columns:        sg.Source.TblCols,
		ColumnNames:    sg.Source.names,
	}
	reader.stats = stats
	reader.SetSchema(schema)
	reader.childrenReqProps = props
	return reader
}

// GetPhysicalIndexReader returns PhysicalIndexReader for logical TiKVSingleGather.
func (sg *TiKVSingleGather) GetPhysicalIndexReader(schema *expression.Schema, stats *property.StatsInfo, props ...*property.PhysicalProperty) *PhysicalIndexReader {
	reader := PhysicalIndexReader{}.Init(sg.ctx, sg.blockOffset)
	reader.stats = stats
	reader.SetSchema(schema)
	reader.childrenReqProps = props
	return reader
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalTableReader) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalTableReader)
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	cloned.StoreType = p.StoreType
	cloned.ReadReqType = p.ReadReqType
	cloned.IsCommonHandle = p.IsCommonHandle
	if cloned.tablePlan, err = p.tablePlan.Clone(); err != nil {
		return nil, err
	}
	if cloned.TablePlans, err = clonePhysicalPlan(p.TablePlans); err != nil {
		return nil, err
	}
	return cloned, nil
}

// SetChildren overrides PhysicalPlan SetChildren interface.
func (p *PhysicalTableReader) SetChildren(children ...PhysicalPlan) {
	p.tablePlan = children[0]
	p.TablePlans = flattenPushDownPlan(p.tablePlan)
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalTableReader) ExtractCorrelatedCols() (corCols []*expression.CorrelatedColumn) {
	for _, child := range p.TablePlans {
		corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(child)...)
	}
	return corCols
}

func (p *PhysicalTableReader) buildPlanTrace() *tracing.PlanTrace {
	rp := p.basePhysicalPlan.buildPlanTrace()
	if p.tablePlan != nil {
		rp.Children = append(rp.Children, p.tablePlan.buildPlanTrace())
	}
	return rp
}

func (p *PhysicalTableReader) appendChildCandidate(op *physicalOptimizeOp) {
	p.basePhysicalPlan.appendChildCandidate(op)
	appendChildCandidate(p, p.tablePlan, op)
}

// PhysicalIndexReader is the index reader in tidb.
type PhysicalIndexReader struct {
	physicalSchemaProducer

	// IndexPlans flats the indexPlan to construct executor pb.
	IndexPlans []PhysicalPlan
	indexPlan  PhysicalPlan

	// OutputColumns represents the columns that index reader should return.
	OutputColumns []*expression.Column

	// Used by partition table.
	PartitionInfo PartitionInfo
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalIndexReader) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalIndexReader)
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	if cloned.indexPlan, err = p.indexPlan.Clone(); err != nil {
		return nil, err
	}
	if cloned.IndexPlans, err = clonePhysicalPlan(p.IndexPlans); err != nil {
		return nil, err
	}
	cloned.OutputColumns = util.CloneCols(p.OutputColumns)
	return cloned, err
}

// SetSchema overrides PhysicalPlan SetSchema interface.
func (p *PhysicalIndexReader) SetSchema(_ *expression.Schema) {
	if p.indexPlan != nil {
		p.IndexPlans = flattenPushDownPlan(p.indexPlan)
		switch p.indexPlan.(type) {
		case *PhysicalHashAgg, *PhysicalStreamAgg, *PhysicalProjection:
			p.schema = p.indexPlan.Schema()
		default:
			is := p.IndexPlans[0].(*PhysicalIndexScan)
			p.schema = is.dataSourceSchema
		}
		p.OutputColumns = p.schema.Clone().Columns
	}
}

// SetChildren overrides PhysicalPlan SetChildren interface.
func (p *PhysicalIndexReader) SetChildren(children ...PhysicalPlan) {
	p.indexPlan = children[0]
	p.SetSchema(nil)
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalIndexReader) ExtractCorrelatedCols() (corCols []*expression.CorrelatedColumn) {
	for _, child := range p.IndexPlans {
		corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(child)...)
	}
	return corCols
}

func (p *PhysicalIndexReader) buildPlanTrace() *tracing.PlanTrace {
	rp := p.basePhysicalPlan.buildPlanTrace()
	if p.indexPlan != nil {
		rp.Children = append(rp.Children, p.indexPlan.buildPlanTrace())
	}
	return rp
}

func (p *PhysicalIndexReader) appendChildCandidate(op *physicalOptimizeOp) {
	p.basePhysicalPlan.appendChildCandidate(op)
	if p.indexPlan != nil {
		appendChildCandidate(p, p.indexPlan, op)
	}
}

// MemoryUsage return the memory usage of PhysicalIndexReader
func (p *PhysicalIndexReader) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + p.PartitionInfo.MemoryUsage()
	if p.indexPlan != nil {
		p.indexPlan.MemoryUsage()
	}

	for _, plan := range p.IndexPlans {
		sum += plan.MemoryUsage()
	}
	for _, col := range p.OutputColumns {
		sum += col.MemoryUsage()
	}
	return
}

// PushedDownLimit is the limit operator pushed down into PhysicalIndexLookUpReader.
type PushedDownLimit struct {
	Offset uint64
	Count  uint64
}

// Clone clones this pushed-down list.
func (p *PushedDownLimit) Clone() *PushedDownLimit {
	cloned := new(PushedDownLimit)
	*cloned = *p
	return cloned
}

const pushedDownLimitSize = size.SizeOfUint64 * 2

// MemoryUsage return the memory usage of PushedDownLimit
func (p *PushedDownLimit) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	return pushedDownLimitSize
}

// PhysicalIndexLookUpReader is the index look up reader in tidb. It's used in case of double reading.
type PhysicalIndexLookUpReader struct {
	physicalSchemaProducer

	// IndexPlans flats the indexPlan to construct executor pb.
	IndexPlans []PhysicalPlan
	// TablePlans flats the tablePlan to construct executor pb.
	TablePlans []PhysicalPlan
	indexPlan  PhysicalPlan
	tablePlan  PhysicalPlan
	Paging     bool

	ExtraHandleCol *expression.Column
	// PushedLimit is used to avoid unnecessary table scan tasks of IndexLookUpReader.
	PushedLimit *PushedDownLimit

	CommonHandleCols []*expression.Column

	// Used by partition table.
	PartitionInfo PartitionInfo

	// required by cost calculation
	expectedCnt uint64
	keepOrder   bool
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalIndexLookUpReader) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalIndexLookUpReader)
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	if cloned.IndexPlans, err = clonePhysicalPlan(p.IndexPlans); err != nil {
		return nil, err
	}
	if cloned.TablePlans, err = clonePhysicalPlan(p.TablePlans); err != nil {
		return nil, err
	}
	if cloned.indexPlan, err = p.indexPlan.Clone(); err != nil {
		return nil, err
	}
	if cloned.tablePlan, err = p.tablePlan.Clone(); err != nil {
		return nil, err
	}
	if p.ExtraHandleCol != nil {
		cloned.ExtraHandleCol = p.ExtraHandleCol.Clone().(*expression.Column)
	}
	if p.PushedLimit != nil {
		cloned.PushedLimit = p.PushedLimit.Clone()
	}
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalIndexLookUpReader) ExtractCorrelatedCols() (corCols []*expression.CorrelatedColumn) {
	for _, child := range p.TablePlans {
		corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(child)...)
	}
	for _, child := range p.IndexPlans {
		corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(child)...)
	}
	return corCols
}

// GetIndexNetDataSize return the estimated total size in bytes via network transfer.
func (p *PhysicalIndexLookUpReader) GetIndexNetDataSize() float64 {
	return getTblStats(p.indexPlan).GetAvgRowSize(p.ctx, p.indexPlan.Schema().Columns, true, false) * p.indexPlan.StatsCount()
}

// GetAvgTableRowSize return the average row size of each final row.
func (p *PhysicalIndexLookUpReader) GetAvgTableRowSize() float64 {
	return getTblStats(p.tablePlan).GetAvgRowSize(p.ctx, p.tablePlan.Schema().Columns, false, false)
}

func (p *PhysicalIndexLookUpReader) buildPlanTrace() *tracing.PlanTrace {
	rp := p.basePhysicalPlan.buildPlanTrace()
	if p.indexPlan != nil {
		rp.Children = append(rp.Children, p.indexPlan.buildPlanTrace())
	}
	if p.tablePlan != nil {
		rp.Children = append(rp.Children, p.tablePlan.buildPlanTrace())
	}
	return rp
}

func (p *PhysicalIndexLookUpReader) appendChildCandidate(op *physicalOptimizeOp) {
	p.basePhysicalPlan.appendChildCandidate(op)
	if p.indexPlan != nil {
		appendChildCandidate(p, p.indexPlan, op)
	}
	if p.tablePlan != nil {
		appendChildCandidate(p, p.tablePlan, op)
	}
}

// MemoryUsage return the memory usage of PhysicalIndexLookUpReader
func (p *PhysicalIndexLookUpReader) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfBool*2 + p.PartitionInfo.MemoryUsage() + size.SizeOfUint64

	if p.indexPlan != nil {
		sum += p.indexPlan.MemoryUsage()
	}
	if p.tablePlan != nil {
		sum += p.tablePlan.MemoryUsage()
	}
	if p.ExtraHandleCol != nil {
		sum += p.ExtraHandleCol.MemoryUsage()
	}
	if p.PushedLimit != nil {
		sum += p.PushedLimit.MemoryUsage()
	}

	// since IndexPlans and TablePlans are the flats of indexPlan and tablePlan, so we don't count it
	for _, col := range p.CommonHandleCols {
		sum += col.MemoryUsage()
	}
	return
}

// PhysicalIndexMergeReader is the reader using multiple indexes in tidb.
type PhysicalIndexMergeReader struct {
	physicalSchemaProducer

	// IsIntersectionType means whether it's intersection type or union type.
	// Intersection type is for expressions connected by `AND` and union type is for `OR`.
	IsIntersectionType bool
	// AccessMVIndex indicates whether this IndexMergeReader access a MVIndex.
	AccessMVIndex bool

	// PushedLimit is used to avoid unnecessary table scan tasks of IndexMergeReader.
	PushedLimit *PushedDownLimit
	// ByItems is used to support sorting the handles returned by partialPlans.
	ByItems []*util.ByItems

	// PartialPlans flats the partialPlans to construct executor pb.
	PartialPlans [][]PhysicalPlan
	// TablePlans flats the tablePlan to construct executor pb.
	TablePlans []PhysicalPlan
	// partialPlans are the partial plans that have not been flatted. The type of each element is permitted PhysicalIndexScan or PhysicalTableScan.
	partialPlans []PhysicalPlan
	// tablePlan is a PhysicalTableScan to get the table tuples. Current, it must be not nil.
	tablePlan PhysicalPlan

	// Used by partition table.
	PartitionInfo PartitionInfo

	KeepOrder bool
}

// GetAvgTableRowSize return the average row size of table plan.
func (p *PhysicalIndexMergeReader) GetAvgTableRowSize() float64 {
	return getTblStats(p.TablePlans[len(p.TablePlans)-1]).GetAvgRowSize(p.SCtx(), p.Schema().Columns, false, false)
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalIndexMergeReader) ExtractCorrelatedCols() (corCols []*expression.CorrelatedColumn) {
	for _, child := range p.TablePlans {
		corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(child)...)
	}
	for _, child := range p.partialPlans {
		corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(child)...)
	}
	for _, PartialPlan := range p.PartialPlans {
		for _, child := range PartialPlan {
			corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(child)...)
		}
	}
	return corCols
}

func (p *PhysicalIndexMergeReader) buildPlanTrace() *tracing.PlanTrace {
	rp := p.basePhysicalPlan.buildPlanTrace()
	if p.tablePlan != nil {
		rp.Children = append(rp.Children, p.tablePlan.buildPlanTrace())
	}
	for _, partialPlan := range p.partialPlans {
		rp.Children = append(rp.Children, partialPlan.buildPlanTrace())
	}
	return rp
}

func (p *PhysicalIndexMergeReader) appendChildCandidate(op *physicalOptimizeOp) {
	p.basePhysicalPlan.appendChildCandidate(op)
	if p.tablePlan != nil {
		appendChildCandidate(p, p.tablePlan, op)
	}
	for _, partialPlan := range p.partialPlans {
		appendChildCandidate(p, partialPlan, op)
	}
}

// MemoryUsage return the memory usage of PhysicalIndexMergeReader
func (p *PhysicalIndexMergeReader) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + p.PartitionInfo.MemoryUsage()
	if p.tablePlan != nil {
		sum += p.tablePlan.MemoryUsage()
	}

	for _, plans := range p.PartialPlans {
		for _, plan := range plans {
			sum += plan.MemoryUsage()
		}
	}
	for _, plan := range p.TablePlans {
		sum += plan.MemoryUsage()
	}
	for _, plan := range p.partialPlans {
		sum += plan.MemoryUsage()
	}
	return
}

// PhysicalIndexScan represents an index scan plan.
type PhysicalIndexScan struct {
	physicalSchemaProducer

	// AccessCondition is used to calculate range.
	AccessCondition []expression.Expression

	Table      *model.TableInfo
	Index      *model.IndexInfo
	IdxCols    []*expression.Column
	IdxColLens []int
	Ranges     []*ranger.Range
	Columns    []*model.ColumnInfo
	DBName     model.CIStr

	TableAsName *model.CIStr

	// dataSourceSchema is the original schema of DataSource. The schema of index scan in KV and index reader in TiDB
	// will be different. The schema of index scan will decode all columns of index but the TiDB only need some of them.
	dataSourceSchema *expression.Schema

	// Hist is the histogram when the query was issued.
	// It is used for query feedback.
	Hist *statistics.Histogram

	rangeInfo string

	// The index scan may be on a partition.
	physicalTableID int64

	GenExprs map[model.TableItemID]expression.Expression

	isPartition bool
	Desc        bool
	KeepOrder   bool
	// ByItems only for partition table with orderBy + pushedLimit
	ByItems []*util.ByItems

	// DoubleRead means if the index executor will read kv two times.
	// If the query requires the columns that don't belong to index, DoubleRead will be true.
	DoubleRead bool

	NeedCommonHandle bool

	// required by cost model
	// tblColHists contains all columns before pruning, which are used to calculate row-size
	tblColHists   *statistics.HistColl
	pkIsHandleCol *expression.Column

	// constColsByCond records the constant part of the index columns caused by the access conds.
	// e.g. the index is (a, b, c) and there's filter a = 1 and b = 2, then the column a and b are const part.
	constColsByCond []bool

	prop *property.PhysicalProperty

	// usedStatsInfo records stats status of this physical table.
	// It's for printing stats related information when display execution plan.
	usedStatsInfo *stmtctx.UsedStatsInfoForTable
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalIndexScan) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalIndexScan)
	*cloned = *p
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	cloned.AccessCondition = util.CloneExprs(p.AccessCondition)
	if p.Table != nil {
		cloned.Table = p.Table.Clone()
	}
	if p.Index != nil {
		cloned.Index = p.Index.Clone()
	}
	cloned.IdxCols = util.CloneCols(p.IdxCols)
	cloned.IdxColLens = make([]int, len(p.IdxColLens))
	copy(cloned.IdxColLens, p.IdxColLens)
	cloned.Ranges = util.CloneRanges(p.Ranges)
	cloned.Columns = util.CloneColInfos(p.Columns)
	if p.dataSourceSchema != nil {
		cloned.dataSourceSchema = p.dataSourceSchema.Clone()
	}
	if p.Hist != nil {
		cloned.Hist = p.Hist.Copy()
	}
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalIndexScan) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(p.AccessCondition))
	for _, expr := range p.AccessCondition {
		corCols = append(corCols, expression.ExtractCorColumns(expr)...)
	}
	return corCols
}

const emptyPhysicalIndexScanSize = int64(unsafe.Sizeof(PhysicalIndexScan{}))

// MemoryUsage return the memory usage of PhysicalIndexScan
func (p *PhysicalIndexScan) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = emptyPhysicalIndexScanSize + p.physicalSchemaProducer.MemoryUsage() + int64(cap(p.IdxColLens))*size.SizeOfInt +
		p.DBName.MemoryUsage() + int64(len(p.rangeInfo)) + int64(len(p.Columns))*model.EmptyColumnInfoSize
	if p.TableAsName != nil {
		sum += p.TableAsName.MemoryUsage()
	}
	if p.pkIsHandleCol != nil {
		sum += p.pkIsHandleCol.MemoryUsage()
	}
	if p.prop != nil {
		sum += p.prop.MemoryUsage()
	}
	if p.dataSourceSchema != nil {
		sum += p.dataSourceSchema.MemoryUsage()
	}
	// slice memory usage
	for _, cond := range p.AccessCondition {
		sum += cond.MemoryUsage()
	}
	for _, col := range p.IdxCols {
		sum += col.MemoryUsage()
	}
	for _, rang := range p.Ranges {
		sum += rang.MemUsage()
	}
	for iid, expr := range p.GenExprs {
		sum += int64(unsafe.Sizeof(iid)) + expr.MemoryUsage()
	}
	return
}

// PhysicalMemTable reads memory table.
type PhysicalMemTable struct {
	physicalSchemaProducer

	DBName         model.CIStr
	Table          *model.TableInfo
	Columns        []*model.ColumnInfo
	Extractor      MemTablePredicateExtractor
	QueryTimeRange QueryTimeRange
}

// MemoryUsage return the memory usage of PhysicalMemTable
func (p *PhysicalMemTable) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + p.DBName.MemoryUsage() + size.SizeOfPointer + size.SizeOfSlice +
		int64(cap(p.Columns))*size.SizeOfPointer + size.SizeOfInterface + p.QueryTimeRange.MemoryUsage()
	return
}

// PhysicalTableScan represents a table scan plan.
type PhysicalTableScan struct {
	physicalSchemaProducer

	// AccessCondition is used to calculate range.
	AccessCondition []expression.Expression
	filterCondition []expression.Expression
	// lateMaterializationFilterCondition is used to record the filter conditions
	// that are pushed down to table scan from selection by late materialization.
	// TODO: remove this field after we support pushing down selection to coprocessor.
	lateMaterializationFilterCondition []expression.Expression

	Table   *model.TableInfo
	Columns []*model.ColumnInfo
	DBName  model.CIStr
	Ranges  []*ranger.Range

	TableAsName *model.CIStr

	// Hist is the histogram when the query was issued.
	// It is used for query feedback.
	Hist *statistics.Histogram

	physicalTableID int64

	rangeInfo string

	// HandleIdx is the index of handle, which is only used for admin check table.
	HandleIdx  []int
	HandleCols HandleCols

	StoreType kv.StoreType

	IsMPPOrBatchCop bool // Used for tiflash PartitionTableScan.

	// The table scan may be a partition, rather than a real table.
	// TODO: clean up this field. After we support dynamic partitioning, table scan
	// works on the whole partition table, and `isPartition` is not used.
	isPartition bool
	// KeepOrder is true, if sort data by scanning pkcol,
	KeepOrder bool
	Desc      bool
	// ByItems only for partition table with orderBy + pushedLimit
	ByItems []*util.ByItems

	isChildOfIndexLookUp bool

	PartitionInfo PartitionInfo

	SampleInfo *TableSampleInfo

	// required by cost model
	// tblCols and tblColHists contains all columns before pruning, which are used to calculate row-size
	tblCols     []*expression.Column
	tblColHists *statistics.HistColl
	prop        *property.PhysicalProperty

	// constColsByCond records the constant part of the index columns caused by the access conds.
	// e.g. the index is (a, b, c) and there's filter a = 1 and b = 2, then the column a and b are const part.
	// it's for indexMerge's tableScan only.
	constColsByCond []bool

	// usedStatsInfo records stats status of this physical table.
	// It's for printing stats related information when display execution plan.
	usedStatsInfo *stmtctx.UsedStatsInfoForTable

	// for runtime filter
	runtimeFilterList []*RuntimeFilter
	maxWaitTimeMs     int
}

// Clone implements PhysicalPlan interface.
func (ts *PhysicalTableScan) Clone() (PhysicalPlan, error) {
	clonedScan := new(PhysicalTableScan)
	*clonedScan = *ts
	prod, err := ts.physicalSchemaProducer.cloneWithSelf(clonedScan)
	if err != nil {
		return nil, err
	}
	clonedScan.physicalSchemaProducer = *prod
	clonedScan.AccessCondition = util.CloneExprs(ts.AccessCondition)
	clonedScan.filterCondition = util.CloneExprs(ts.filterCondition)
	if ts.Table != nil {
		clonedScan.Table = ts.Table.Clone()
	}
	clonedScan.Columns = util.CloneColInfos(ts.Columns)
	clonedScan.Ranges = util.CloneRanges(ts.Ranges)
	clonedScan.TableAsName = ts.TableAsName
	if ts.Hist != nil {
		clonedScan.Hist = ts.Hist.Copy()
	}
	clonedScan.rangeInfo = ts.rangeInfo
	clonedScan.runtimeFilterList = make([]*RuntimeFilter, len(ts.runtimeFilterList))
	for i, rf := range ts.runtimeFilterList {
		clonedRF := rf.Clone()
		clonedScan.runtimeFilterList[i] = clonedRF
	}
	return clonedScan, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (ts *PhysicalTableScan) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(ts.AccessCondition)+len(ts.filterCondition))
	for _, expr := range ts.AccessCondition {
		corCols = append(corCols, expression.ExtractCorColumns(expr)...)
	}
	for _, expr := range ts.filterCondition {
		corCols = append(corCols, expression.ExtractCorColumns(expr)...)
	}
	return corCols
}

// IsPartition returns true and partition ID if it's actually a partition.
func (ts *PhysicalTableScan) IsPartition() (bool, int64) {
	return ts.isPartition, ts.physicalTableID
}

// ResolveCorrelatedColumns resolves the correlated columns in range access.
// We already limit range mem usage when building ranges in optimizer phase, so we don't need and shouldn't limit range
// mem usage when rebuilding ranges during the execution phase.
func (ts *PhysicalTableScan) ResolveCorrelatedColumns() ([]*ranger.Range, error) {
	access := ts.AccessCondition
	if ts.Table.IsCommonHandle {
		pkIdx := tables.FindPrimaryIndex(ts.Table)
		idxCols, idxColLens := expression.IndexInfo2PrefixCols(ts.Columns, ts.Schema().Columns, pkIdx)
		for _, cond := range access {
			newCond, err := expression.SubstituteCorCol2Constant(cond)
			if err != nil {
				return nil, err
			}
			access = append(access, newCond)
		}
		// All of access conditions must be used to build ranges, so we don't limit range memory usage.
		res, err := ranger.DetachCondAndBuildRangeForIndex(ts.SCtx(), access, idxCols, idxColLens, 0)
		if err != nil {
			return nil, err
		}
		ts.Ranges = res.Ranges
	} else {
		var err error
		pkTP := ts.Table.GetPkColInfo().FieldType
		// All of access conditions must be used to build ranges, so we don't limit range memory usage.
		ts.Ranges, _, _, err = ranger.BuildTableRange(access, ts.SCtx(), &pkTP, 0)
		if err != nil {
			return nil, err
		}
	}
	return ts.Ranges, nil
}

// ExpandVirtualColumn expands the virtual column's dependent columns to ts's schema and column.
func ExpandVirtualColumn(columns []*model.ColumnInfo, schema *expression.Schema,
	colsInfo []*model.ColumnInfo) []*model.ColumnInfo {
	copyColumn := make([]*model.ColumnInfo, len(columns))
	copy(copyColumn, columns)
	var extraColumn *expression.Column
	var extraColumnModel *model.ColumnInfo
	if schema.Columns[len(schema.Columns)-1].ID == model.ExtraHandleID {
		extraColumn = schema.Columns[len(schema.Columns)-1]
		extraColumnModel = copyColumn[len(copyColumn)-1]
		schema.Columns = schema.Columns[:len(schema.Columns)-1]
		copyColumn = copyColumn[:len(copyColumn)-1]
	}
	schemaColumns := schema.Columns
	for _, col := range schemaColumns {
		if col.VirtualExpr == nil {
			continue
		}

		baseCols := expression.ExtractDependentColumns(col.VirtualExpr)
		for _, baseCol := range baseCols {
			if !schema.Contains(baseCol) {
				schema.Columns = append(schema.Columns, baseCol)
				copyColumn = append(copyColumn, FindColumnInfoByID(colsInfo, baseCol.ID)) // nozero
			}
		}
	}
	if extraColumn != nil {
		schema.Columns = append(schema.Columns, extraColumn)
		copyColumn = append(copyColumn, extraColumnModel) // nozero
	}
	return copyColumn
}

// SetIsChildOfIndexLookUp is to set the bool if is a child of IndexLookUpReader
func (ts *PhysicalTableScan) SetIsChildOfIndexLookUp(isIsChildOfIndexLookUp bool) {
	ts.isChildOfIndexLookUp = isIsChildOfIndexLookUp
}

const emptyPhysicalTableScanSize = int64(unsafe.Sizeof(PhysicalTableScan{}))

// MemoryUsage return the memory usage of PhysicalTableScan
func (ts *PhysicalTableScan) MemoryUsage() (sum int64) {
	if ts == nil {
		return
	}

	sum = emptyPhysicalTableScanSize + ts.physicalSchemaProducer.MemoryUsage() + ts.DBName.MemoryUsage() +
		int64(cap(ts.HandleIdx))*size.SizeOfInt + ts.PartitionInfo.MemoryUsage() + int64(len(ts.rangeInfo))
	if ts.TableAsName != nil {
		sum += ts.TableAsName.MemoryUsage()
	}
	if ts.HandleCols != nil {
		sum += ts.HandleCols.MemoryUsage()
	}
	if ts.prop != nil {
		sum += ts.prop.MemoryUsage()
	}
	// slice memory usage
	for _, cond := range ts.AccessCondition {
		sum += cond.MemoryUsage()
	}
	for _, cond := range ts.filterCondition {
		sum += cond.MemoryUsage()
	}
	for _, rang := range ts.Ranges {
		sum += rang.MemUsage()
	}
	for _, col := range ts.tblCols {
		sum += col.MemoryUsage()
	}
	return
}

// PhysicalProjection is the physical operator of projection.
type PhysicalProjection struct {
	physicalSchemaProducer

	Exprs                []expression.Expression
	CalculateNoDelay     bool
	AvoidColumnEvaluator bool
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalProjection) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalProjection)
	*cloned = *p
	base, err := p.basePhysicalPlan.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalPlan = *base
	cloned.Exprs = util.CloneExprs(p.Exprs)
	return cloned, err
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalProjection) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(p.Exprs))
	for _, expr := range p.Exprs {
		corCols = append(corCols, expression.ExtractCorColumns(expr)...)
	}
	return corCols
}

// MemoryUsage return the memory usage of PhysicalProjection
func (p *PhysicalProjection) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalPlan.MemoryUsage() + size.SizeOfBool*2
	for _, expr := range p.Exprs {
		sum += expr.MemoryUsage()
	}
	return
}

// PhysicalTopN is the physical operator of topN.
type PhysicalTopN struct {
	basePhysicalPlan

	ByItems     []*util.ByItems
	PartitionBy []property.SortItem
	Offset      uint64
	Count       uint64
}

// GetPartitionBy returns partition by fields
func (lt *PhysicalTopN) GetPartitionBy() []property.SortItem {
	return lt.PartitionBy
}

// Clone implements PhysicalPlan interface.
func (lt *PhysicalTopN) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalTopN)
	*cloned = *lt
	base, err := lt.basePhysicalPlan.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalPlan = *base
	cloned.ByItems = make([]*util.ByItems, 0, len(lt.ByItems))
	for _, it := range lt.ByItems {
		cloned.ByItems = append(cloned.ByItems, it.Clone())
	}
	cloned.PartitionBy = make([]property.SortItem, 0, len(lt.PartitionBy))
	for _, it := range lt.PartitionBy {
		cloned.PartitionBy = append(cloned.PartitionBy, it.Clone())
	}
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (lt *PhysicalTopN) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(lt.ByItems))
	for _, item := range lt.ByItems {
		corCols = append(corCols, expression.ExtractCorColumns(item.Expr)...)
	}
	return corCols
}

// MemoryUsage return the memory usage of PhysicalTopN
func (lt *PhysicalTopN) MemoryUsage() (sum int64) {
	if lt == nil {
		return
	}

	sum = lt.basePhysicalPlan.MemoryUsage() + size.SizeOfSlice + int64(cap(lt.ByItems))*size.SizeOfPointer + size.SizeOfUint64*2
	for _, byItem := range lt.ByItems {
		sum += byItem.MemoryUsage()
	}
	for _, item := range lt.PartitionBy {
		sum += item.MemoryUsage()
	}
	return
}

// PhysicalApply represents apply plan, only used for subquery.
type PhysicalApply struct {
	PhysicalHashJoin

	CanUseCache bool
	Concurrency int
	OuterSchema []*expression.CorrelatedColumn
}

// Clone implements PhysicalPlan interface.
func (la *PhysicalApply) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalApply)
	base, err := la.PhysicalHashJoin.Clone()
	if err != nil {
		return nil, err
	}
	hj := base.(*PhysicalHashJoin)
	cloned.PhysicalHashJoin = *hj
	cloned.CanUseCache = la.CanUseCache
	cloned.Concurrency = la.Concurrency
	for _, col := range la.OuterSchema {
		cloned.OuterSchema = append(cloned.OuterSchema, col.Clone().(*expression.CorrelatedColumn))
	}
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (la *PhysicalApply) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := la.PhysicalHashJoin.ExtractCorrelatedCols()
	for i := len(corCols) - 1; i >= 0; i-- {
		if la.children[0].Schema().Contains(&corCols[i].Column) {
			corCols = append(corCols[:i], corCols[i+1:]...)
		}
	}
	return corCols
}

// MemoryUsage return the memory usage of PhysicalApply
func (la *PhysicalApply) MemoryUsage() (sum int64) {
	if la == nil {
		return
	}

	sum = la.PhysicalHashJoin.MemoryUsage() + size.SizeOfBool + size.SizeOfBool + size.SizeOfSlice +
		int64(cap(la.OuterSchema))*size.SizeOfPointer
	for _, corrCol := range la.OuterSchema {
		sum += corrCol.MemoryUsage()
	}
	return
}

type basePhysicalJoin struct {
	physicalSchemaProducer

	JoinType JoinType

	LeftConditions  expression.CNFExprs
	RightConditions expression.CNFExprs
	OtherConditions expression.CNFExprs

	InnerChildIdx int
	OuterJoinKeys []*expression.Column
	InnerJoinKeys []*expression.Column
	LeftJoinKeys  []*expression.Column
	RightJoinKeys []*expression.Column
	// IsNullEQ is used for cases like Except statement where null key should be matched with null key.
	// <1,null> is exactly matched with <1,null>, where the null value should not be filtered and
	// the null is exactly matched with null only. (while in NAAJ null value should also be matched
	// with other non-null item as well)
	IsNullEQ      []bool
	DefaultValues []types.Datum

	LeftNAJoinKeys  []*expression.Column
	RightNAJoinKeys []*expression.Column
}

func (p *basePhysicalJoin) getInnerChildIdx() int {
	return p.InnerChildIdx
}

func (p *basePhysicalJoin) cloneWithSelf(newSelf PhysicalPlan) (*basePhysicalJoin, error) {
	cloned := new(basePhysicalJoin)
	base, err := p.physicalSchemaProducer.cloneWithSelf(newSelf)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	cloned.JoinType = p.JoinType
	cloned.LeftConditions = util.CloneExprs(p.LeftConditions)
	cloned.RightConditions = util.CloneExprs(p.RightConditions)
	cloned.OtherConditions = util.CloneExprs(p.OtherConditions)
	cloned.InnerChildIdx = p.InnerChildIdx
	cloned.OuterJoinKeys = util.CloneCols(p.OuterJoinKeys)
	cloned.InnerJoinKeys = util.CloneCols(p.InnerJoinKeys)
	cloned.LeftJoinKeys = util.CloneCols(p.LeftJoinKeys)
	cloned.RightJoinKeys = util.CloneCols(p.RightJoinKeys)
	cloned.LeftNAJoinKeys = util.CloneCols(p.LeftNAJoinKeys)
	cloned.RightNAJoinKeys = util.CloneCols(p.RightNAJoinKeys)
	for _, d := range p.DefaultValues {
		cloned.DefaultValues = append(cloned.DefaultValues, *d.Clone())
	}
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *basePhysicalJoin) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(p.LeftConditions)+len(p.RightConditions)+len(p.OtherConditions))
	for _, fun := range p.LeftConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	for _, fun := range p.RightConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	for _, fun := range p.OtherConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	return corCols
}

const emptyBasePhysicalJoinSize = int64(unsafe.Sizeof(basePhysicalJoin{}))

// MemoryUsage return the memory usage of basePhysicalJoin
func (p *basePhysicalJoin) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = emptyBasePhysicalJoinSize + p.physicalSchemaProducer.MemoryUsage() + int64(cap(p.IsNullEQ))*size.SizeOfBool +
		int64(cap(p.LeftConditions)+cap(p.RightConditions)+cap(p.OtherConditions))*size.SizeOfInterface +
		int64(cap(p.OuterJoinKeys)+cap(p.InnerJoinKeys)+cap(p.LeftJoinKeys)+cap(p.RightNAJoinKeys)+cap(p.LeftNAJoinKeys)+
			cap(p.RightNAJoinKeys))*size.SizeOfPointer + int64(cap(p.DefaultValues))*types.EmptyDatumSize

	for _, cond := range p.LeftConditions {
		sum += cond.MemoryUsage()
	}
	for _, cond := range p.RightConditions {
		sum += cond.MemoryUsage()
	}
	for _, cond := range p.OtherConditions {
		sum += cond.MemoryUsage()
	}
	for _, col := range p.LeftJoinKeys {
		sum += col.MemoryUsage()
	}
	for _, col := range p.RightJoinKeys {
		sum += col.MemoryUsage()
	}
	for _, col := range p.InnerJoinKeys {
		sum += col.MemoryUsage()
	}
	for _, col := range p.OuterJoinKeys {
		sum += col.MemoryUsage()
	}
	for _, datum := range p.DefaultValues {
		sum += datum.MemUsage()
	}
	for _, col := range p.LeftNAJoinKeys {
		sum += col.MemoryUsage()
	}
	for _, col := range p.RightNAJoinKeys {
		sum += col.MemoryUsage()
	}
	return
}

// PhysicalHashJoin represents hash join implementation of LogicalJoin.
type PhysicalHashJoin struct {
	basePhysicalJoin

	Concurrency     uint
	EqualConditions []*expression.ScalarFunction

	// null aware equal conditions
	NAEqualConditions []*expression.ScalarFunction

	// use the outer table to build a hash table when the outer table is smaller.
	UseOuterToBuild bool

	// on which store the join executes.
	storeTp        kv.StoreType
	mppShuffleJoin bool

	// for runtime filter
	runtimeFilterList []*RuntimeFilter
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalHashJoin) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalHashJoin)
	base, err := p.basePhysicalJoin.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalJoin = *base
	cloned.Concurrency = p.Concurrency
	cloned.UseOuterToBuild = p.UseOuterToBuild
	for _, c := range p.EqualConditions {
		cloned.EqualConditions = append(cloned.EqualConditions, c.Clone().(*expression.ScalarFunction))
	}
	for _, c := range p.NAEqualConditions {
		cloned.NAEqualConditions = append(cloned.NAEqualConditions, c.Clone().(*expression.ScalarFunction))
	}
	for _, rf := range p.runtimeFilterList {
		clonedRF := rf.Clone()
		cloned.runtimeFilterList = append(cloned.runtimeFilterList, clonedRF)
	}
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalHashJoin) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(p.EqualConditions)+len(p.NAEqualConditions)+len(p.LeftConditions)+len(p.RightConditions)+len(p.OtherConditions))
	for _, fun := range p.EqualConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	for _, fun := range p.NAEqualConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	for _, fun := range p.LeftConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	for _, fun := range p.RightConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	for _, fun := range p.OtherConditions {
		corCols = append(corCols, expression.ExtractCorColumns(fun)...)
	}
	return corCols
}

// MemoryUsage return the memory usage of PhysicalHashJoin
func (p *PhysicalHashJoin) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalJoin.MemoryUsage() + size.SizeOfUint + size.SizeOfSlice + size.SizeOfBool*2 + size.SizeOfUint8

	for _, expr := range p.EqualConditions {
		sum += expr.MemoryUsage()
	}
	for _, expr := range p.NAEqualConditions {
		sum += expr.MemoryUsage()
	}
	return
}

// RightIsBuildSide return true when right side is build side
func (p *PhysicalHashJoin) RightIsBuildSide() bool {
	if p.UseOuterToBuild {
		return p.InnerChildIdx == 0
	}
	return p.InnerChildIdx != 0
}

// NewPhysicalHashJoin creates a new PhysicalHashJoin from LogicalJoin.
func NewPhysicalHashJoin(p *LogicalJoin, innerIdx int, useOuterToBuild bool, newStats *property.StatsInfo, prop ...*property.PhysicalProperty) *PhysicalHashJoin {
	leftJoinKeys, rightJoinKeys, isNullEQ, _ := p.GetJoinKeys()
	leftNAJoinKeys, rightNAJoinKeys := p.GetNAJoinKeys()
	baseJoin := basePhysicalJoin{
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		LeftJoinKeys:    leftJoinKeys,
		RightJoinKeys:   rightJoinKeys,
		// NA join keys
		LeftNAJoinKeys:  leftNAJoinKeys,
		RightNAJoinKeys: rightNAJoinKeys,
		IsNullEQ:        isNullEQ,
		JoinType:        p.JoinType,
		DefaultValues:   p.DefaultValues,
		InnerChildIdx:   innerIdx,
	}
	hashJoin := PhysicalHashJoin{
		basePhysicalJoin:  baseJoin,
		EqualConditions:   p.EqualConditions,
		NAEqualConditions: p.NAEQConditions,
		Concurrency:       uint(p.ctx.GetSessionVars().HashJoinConcurrency()),
		UseOuterToBuild:   useOuterToBuild,
	}.Init(p.ctx, newStats, p.blockOffset, prop...)
	return hashJoin
}

// PhysicalIndexJoin represents the plan of index look up join.
type PhysicalIndexJoin struct {
	basePhysicalJoin

	innerTask task

	// Ranges stores the IndexRanges when the inner plan is index scan.
	Ranges ranger.MutableRanges
	// KeyOff2IdxOff maps the offsets in join key to the offsets in the index.
	KeyOff2IdxOff []int
	// IdxColLens stores the length of each index column.
	IdxColLens []int
	// CompareFilters stores the filters for last column if those filters need to be evaluated during execution.
	// e.g. select * from t, t1 where t.a = t1.a and t.b > t1.b and t.b < t1.b+10
	//      If there's index(t.a, t.b). All the filters can be used to construct index range but t.b > t1.b and t.b < t1.b+10
	//      need to be evaluated after we fetch the data of t1.
	// This struct stores them and evaluate them to ranges.
	CompareFilters *ColWithCmpFuncManager
	// OuterHashKeys indicates the outer keys used to build hash table during
	// execution. OuterJoinKeys is the prefix of OuterHashKeys.
	OuterHashKeys []*expression.Column
	// InnerHashKeys indicates the inner keys used to build hash table during
	// execution. InnerJoinKeys is the prefix of InnerHashKeys.
	InnerHashKeys []*expression.Column
}

// MemoryUsage return the memory usage of PhysicalIndexJoin
func (p *PhysicalIndexJoin) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalJoin.MemoryUsage() + size.SizeOfInterface*2 + size.SizeOfSlice*4 +
		int64(cap(p.KeyOff2IdxOff)+cap(p.IdxColLens))*size.SizeOfInt + size.SizeOfPointer
	if p.innerTask != nil {
		sum += p.innerTask.MemoryUsage()
	}
	if p.CompareFilters != nil {
		sum += p.CompareFilters.MemoryUsage()
	}

	for _, col := range p.OuterHashKeys {
		sum += col.MemoryUsage()
	}
	for _, col := range p.InnerHashKeys {
		sum += col.MemoryUsage()
	}
	return
}

// PhysicalIndexMergeJoin represents the plan of index look up merge join.
type PhysicalIndexMergeJoin struct {
	PhysicalIndexJoin

	// KeyOff2KeyOffOrderByIdx maps the offsets in join keys to the offsets in join keys order by index.
	KeyOff2KeyOffOrderByIdx []int
	// CompareFuncs store the compare functions for outer join keys and inner join key.
	CompareFuncs []expression.CompareFunc
	// OuterCompareFuncs store the compare functions for outer join keys and outer join
	// keys, it's for outer rows sort's convenience.
	OuterCompareFuncs []expression.CompareFunc
	// NeedOuterSort means whether outer rows should be sorted to build range.
	NeedOuterSort bool
	// Desc means whether inner child keep desc order.
	Desc bool
}

// MemoryUsage return the memory usage of PhysicalIndexMergeJoin
func (p *PhysicalIndexMergeJoin) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.PhysicalIndexJoin.MemoryUsage() + size.SizeOfSlice*3 + int64(cap(p.KeyOff2KeyOffOrderByIdx))*size.SizeOfInt +
		int64(cap(p.CompareFuncs)+cap(p.OuterCompareFuncs))*size.SizeOfFunc + size.SizeOfBool*2
	return
}

// PhysicalIndexHashJoin represents the plan of index look up hash join.
type PhysicalIndexHashJoin struct {
	PhysicalIndexJoin
	// KeepOuterOrder indicates whether keeping the output result order as the
	// outer side.
	KeepOuterOrder bool
}

// MemoryUsage return the memory usage of PhysicalIndexHashJoin
func (p *PhysicalIndexHashJoin) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	return p.PhysicalIndexJoin.MemoryUsage() + size.SizeOfBool
}

// PhysicalMergeJoin represents merge join implementation of LogicalJoin.
type PhysicalMergeJoin struct {
	basePhysicalJoin

	CompareFuncs []expression.CompareFunc
	// Desc means whether inner child keep desc order.
	Desc bool
}

// MemoryUsage return the memory usage of PhysicalMergeJoin
func (p *PhysicalMergeJoin) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalJoin.MemoryUsage() + size.SizeOfSlice + int64(cap(p.CompareFuncs))*size.SizeOfFunc + size.SizeOfBool
	return
}

// PhysicalExchangeReceiver accepts connection and receives data passively.
type PhysicalExchangeReceiver struct {
	basePhysicalPlan

	Tasks []*kv.MPPTask
	frags []*Fragment

	IsCTEReader bool
}

// Clone implment PhysicalPlan interface.
func (p *PhysicalExchangeReceiver) Clone() (PhysicalPlan, error) {
	np := new(PhysicalExchangeReceiver)
	base, err := p.basePhysicalPlan.cloneWithSelf(np)
	if err != nil {
		return nil, errors.Trace(err)
	}
	np.basePhysicalPlan = *base

	np.IsCTEReader = p.IsCTEReader
	return np, nil
}

// GetExchangeSender return the connected sender of this receiver. We assume that its child must be a receiver.
func (p *PhysicalExchangeReceiver) GetExchangeSender() *PhysicalExchangeSender {
	return p.children[0].(*PhysicalExchangeSender)
}

// MemoryUsage return the memory usage of PhysicalExchangeReceiver
func (p *PhysicalExchangeReceiver) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalPlan.MemoryUsage() + size.SizeOfSlice*2 + int64(cap(p.Tasks)+cap(p.frags))*size.SizeOfPointer
	for _, frag := range p.frags {
		sum += frag.MemoryUsage()
	}
	return
}

// PhysicalExpand is used to expand underlying data sources to feed different grouping sets.
type PhysicalExpand struct {
	// data after repeat-OP will generate a new grouping-ID column to indicate what grouping set is it for.
	physicalSchemaProducer

	// generated grouping ID column itself.
	GroupingIDCol *expression.Column

	// GroupingSets is used to define what kind of group layout should the underlying data follow.
	// For simple case: select count(distinct a), count(distinct b) from t; the grouping expressions are [a] and [b].
	GroupingSets expression.GroupingSets

	// The level projections is generated from grouping sets，make execution more clearly.
	LevelExprs [][]expression.Expression

	// The generated column names. Eg: "grouping_id" and so on.
	ExtraGroupingColNames []string
}

// Init only assigns type and context.
func (p PhysicalExpand) Init(ctx sessionctx.Context, stats *property.StatsInfo, offset int, props ...*property.PhysicalProperty) *PhysicalExpand {
	p.basePhysicalPlan = newBasePhysicalPlan(ctx, plancodec.TypeExpand, &p, offset)
	p.childrenReqProps = props
	p.stats = stats
	return &p
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalExpand) Clone() (PhysicalPlan, error) {
	np := new(PhysicalExpand)
	base, err := p.basePhysicalPlan.cloneWithSelf(np)
	if err != nil {
		return nil, errors.Trace(err)
	}
	np.basePhysicalPlan = *base
	// clone ID cols.
	np.GroupingIDCol = p.GroupingIDCol.Clone().(*expression.Column)

	// clone grouping expressions.
	clonedGroupingSets := make([]expression.GroupingSet, 0, len(p.GroupingSets))
	for _, one := range p.GroupingSets {
		clonedGroupingSets = append(clonedGroupingSets, one.Clone())
	}
	np.GroupingSets = p.GroupingSets
	return np, nil
}

// MemoryUsage return the memory usage of PhysicalExpand
func (p *PhysicalExpand) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfSlice + int64(cap(p.GroupingSets))*size.SizeOfPointer
	for _, gs := range p.GroupingSets {
		sum += gs.MemoryUsage()
	}
	sum += p.GroupingIDCol.MemoryUsage()
	return
}

// PhysicalExchangeSender dispatches data to upstream tasks. That means push mode processing.
type PhysicalExchangeSender struct {
	basePhysicalPlan

	TargetTasks          []*kv.MPPTask
	TargetCTEReaderTasks [][]*kv.MPPTask
	ExchangeType         tipb.ExchangeType
	HashCols             []*property.MPPPartitionColumn
	// Tasks is the mpp task for current PhysicalExchangeSender.
	Tasks           []*kv.MPPTask
	CompressionMode kv.ExchangeCompressionMode
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalExchangeSender) Clone() (PhysicalPlan, error) {
	np := new(PhysicalExchangeSender)
	base, err := p.basePhysicalPlan.cloneWithSelf(np)
	if err != nil {
		return nil, errors.Trace(err)
	}
	np.basePhysicalPlan = *base
	np.ExchangeType = p.ExchangeType
	np.HashCols = p.HashCols
	np.CompressionMode = p.CompressionMode
	return np, nil
}

// MemoryUsage return the memory usage of PhysicalExchangeSender
func (p *PhysicalExchangeSender) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalPlan.MemoryUsage() + size.SizeOfSlice*3 + size.SizeOfInt32 +
		int64(cap(p.TargetTasks)+cap(p.HashCols)+cap(p.Tasks))*size.SizeOfPointer
	for _, hCol := range p.HashCols {
		sum += hCol.MemoryUsage()
	}
	return
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalMergeJoin) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalMergeJoin)
	base, err := p.basePhysicalJoin.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalJoin = *base
	cloned.CompareFuncs = append(cloned.CompareFuncs, p.CompareFuncs...)
	cloned.Desc = p.Desc
	return cloned, nil
}

// PhysicalLock is the physical operator of lock, which is used for `select ... for update` clause.
type PhysicalLock struct {
	basePhysicalPlan

	Lock *ast.SelectLockInfo

	TblID2Handle       map[int64][]HandleCols
	TblID2PhysTblIDCol map[int64]*expression.Column
}

// MemoryUsage return the memory usage of PhysicalLock
func (pl *PhysicalLock) MemoryUsage() (sum int64) {
	if pl == nil {
		return
	}

	sum = pl.basePhysicalPlan.MemoryUsage() + size.SizeOfPointer + size.SizeOfMap*2
	if pl.Lock != nil {
		sum += int64(unsafe.Sizeof(ast.SelectLockInfo{}))
	}

	for _, vals := range pl.TblID2Handle {
		sum += size.SizeOfInt64 + size.SizeOfSlice + int64(cap(vals))*size.SizeOfInterface
		for _, val := range vals {
			sum += val.MemoryUsage()
		}
	}
	for _, val := range pl.TblID2PhysTblIDCol {
		sum += size.SizeOfInt64 + size.SizeOfPointer + val.MemoryUsage()
	}
	return
}

// PhysicalLimit is the physical operator of Limit.
type PhysicalLimit struct {
	physicalSchemaProducer

	PartitionBy []property.SortItem
	Offset      uint64
	Count       uint64
}

// GetPartitionBy returns partition by fields
func (p *PhysicalLimit) GetPartitionBy() []property.SortItem {
	return p.PartitionBy
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalLimit) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalLimit)
	*cloned = *p
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.PartitionBy = make([]property.SortItem, 0, len(p.PartitionBy))
	for _, it := range p.PartitionBy {
		cloned.PartitionBy = append(cloned.PartitionBy, it.Clone())
	}
	cloned.physicalSchemaProducer = *base
	return cloned, nil
}

// MemoryUsage return the memory usage of PhysicalLimit
func (p *PhysicalLimit) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfUint64*2
	return
}

// PhysicalUnionAll is the physical operator of UnionAll.
type PhysicalUnionAll struct {
	physicalSchemaProducer

	mpp bool
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalUnionAll) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalUnionAll)
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	return cloned, nil
}

// MemoryUsage return the memory usage of PhysicalUnionAll
func (p *PhysicalUnionAll) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	return p.physicalSchemaProducer.MemoryUsage() + size.SizeOfBool
}

// AggMppRunMode defines the running mode of aggregation in MPP
type AggMppRunMode int

const (
	// NoMpp means the default value which does not run in MPP
	NoMpp AggMppRunMode = iota
	// Mpp1Phase runs only 1 phase but requires its child's partition property
	Mpp1Phase
	// Mpp2Phase runs partial agg + final agg with hash partition
	Mpp2Phase
	// MppTiDB runs agg on TiDB (and a partial agg on TiFlash if in 2 phase agg)
	MppTiDB
	// MppScalar also has 2 phases. The second phase runs in a single task.
	MppScalar
)

type basePhysicalAgg struct {
	physicalSchemaProducer

	AggFuncs         []*aggregation.AggFuncDesc
	GroupByItems     []expression.Expression
	MppRunMode       AggMppRunMode
	MppPartitionCols []*property.MPPPartitionColumn
}

func (p *basePhysicalAgg) IsFinalAgg() bool {
	if len(p.AggFuncs) > 0 {
		if p.AggFuncs[0].Mode == aggregation.FinalMode || p.AggFuncs[0].Mode == aggregation.CompleteMode {
			return true
		}
	}
	return false
}

func (p *basePhysicalAgg) cloneWithSelf(newSelf PhysicalPlan) (*basePhysicalAgg, error) {
	cloned := new(basePhysicalAgg)
	base, err := p.physicalSchemaProducer.cloneWithSelf(newSelf)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	for _, aggDesc := range p.AggFuncs {
		cloned.AggFuncs = append(cloned.AggFuncs, aggDesc.Clone())
	}
	cloned.GroupByItems = util.CloneExprs(p.GroupByItems)
	return cloned, nil
}

func (p *basePhysicalAgg) numDistinctFunc() (num int) {
	for _, fun := range p.AggFuncs {
		if fun.HasDistinct {
			num++
		}
	}
	return
}

func (p *basePhysicalAgg) getAggFuncCostFactor(isMPP bool) (factor float64) {
	factor = 0.0
	for _, agg := range p.AggFuncs {
		if fac, ok := aggFuncFactor[agg.Name]; ok {
			factor += fac
		} else {
			factor += aggFuncFactor["default"]
		}
	}
	if factor == 0 {
		if isMPP {
			// The default factor 1.0 will lead to 1-phase agg in pseudo stats settings.
			// But in mpp cases, 2-phase is more usual. So we change this factor.
			// TODO: This is still a little tricky and might cause regression. We should
			// calibrate these factors and polish our cost model in the future.
			factor = aggFuncFactor[ast.AggFuncFirstRow]
		} else {
			factor = 1.0
		}
	}
	return
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *basePhysicalAgg) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(p.GroupByItems)+len(p.AggFuncs))
	for _, expr := range p.GroupByItems {
		corCols = append(corCols, expression.ExtractCorColumns(expr)...)
	}
	for _, fun := range p.AggFuncs {
		for _, arg := range fun.Args {
			corCols = append(corCols, expression.ExtractCorColumns(arg)...)
		}
	}
	return corCols
}

// MemoryUsage return the memory usage of basePhysicalAgg
func (p *basePhysicalAgg) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfInt

	for _, agg := range p.AggFuncs {
		sum += agg.MemoryUsage()
	}
	for _, expr := range p.GroupByItems {
		sum += expr.MemoryUsage()
	}
	for _, mppCol := range p.MppPartitionCols {
		sum += mppCol.MemoryUsage()
	}
	return
}

// PhysicalHashAgg is hash operator of aggregate.
type PhysicalHashAgg struct {
	basePhysicalAgg
}

func (p *PhysicalHashAgg) getPointer() *basePhysicalAgg {
	return &p.basePhysicalAgg
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalHashAgg) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalHashAgg)
	base, err := p.basePhysicalAgg.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalAgg = *base
	return cloned, nil
}

// MemoryUsage return the memory usage of PhysicalHashAgg
func (p *PhysicalHashAgg) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	return p.basePhysicalAgg.MemoryUsage()
}

// NewPhysicalHashAgg creates a new PhysicalHashAgg from a LogicalAggregation.
func NewPhysicalHashAgg(la *LogicalAggregation, newStats *property.StatsInfo, prop *property.PhysicalProperty) *PhysicalHashAgg {
	newGbyItems := make([]expression.Expression, len(la.GroupByItems))
	copy(newGbyItems, la.GroupByItems)
	newAggFuncs := make([]*aggregation.AggFuncDesc, len(la.AggFuncs))
	// There's some places that rewrites the aggFunc in-place.
	// I clone it first.
	// It needs a well refactor to make sure that the physical optimize should not change the things of logical plan.
	// It's bad for cascades
	for i, aggFunc := range la.AggFuncs {
		newAggFuncs[i] = aggFunc.Clone()
	}
	agg := basePhysicalAgg{
		GroupByItems: newGbyItems,
		AggFuncs:     newAggFuncs,
	}.initForHash(la.ctx, newStats, la.blockOffset, prop)
	return agg
}

// PhysicalStreamAgg is stream operator of aggregate.
type PhysicalStreamAgg struct {
	basePhysicalAgg
}

func (p *PhysicalStreamAgg) getPointer() *basePhysicalAgg {
	return &p.basePhysicalAgg
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalStreamAgg) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalStreamAgg)
	base, err := p.basePhysicalAgg.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalAgg = *base
	return cloned, nil
}

// MemoryUsage return the memory usage of PhysicalStreamAgg
func (p *PhysicalStreamAgg) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	return p.basePhysicalAgg.MemoryUsage()
}

// PhysicalSort is the physical operator of sort, which implements a memory sort.
type PhysicalSort struct {
	basePhysicalPlan

	ByItems []*util.ByItems
	// whether this operator only need to sort the data of one partition.
	// it is true only if it is used to sort the sharded data of the window function.
	IsPartialSort bool
}

// Clone implements PhysicalPlan interface.
func (ls *PhysicalSort) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalSort)
	cloned.IsPartialSort = ls.IsPartialSort
	base, err := ls.basePhysicalPlan.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalPlan = *base
	for _, it := range ls.ByItems {
		cloned.ByItems = append(cloned.ByItems, it.Clone())
	}
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (ls *PhysicalSort) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(ls.ByItems))
	for _, item := range ls.ByItems {
		corCols = append(corCols, expression.ExtractCorColumns(item.Expr)...)
	}
	return corCols
}

// MemoryUsage return the memory usage of PhysicalSort
func (ls *PhysicalSort) MemoryUsage() (sum int64) {
	if ls == nil {
		return
	}

	sum = ls.basePhysicalPlan.MemoryUsage() + size.SizeOfSlice + int64(cap(ls.ByItems))*size.SizeOfPointer +
		size.SizeOfBool
	for _, byItem := range ls.ByItems {
		sum += byItem.MemoryUsage()
	}
	return
}

// NominalSort asks sort properties for its child. It is a fake operator that will not
// appear in final physical operator tree. It will be eliminated or converted to Projection.
type NominalSort struct {
	basePhysicalPlan

	// These two fields are used to switch ScalarFunctions to Constants. For these
	// NominalSorts, we need to converted to Projections check if the ScalarFunctions
	// are out of bounds. (issue #11653)
	ByItems    []*util.ByItems
	OnlyColumn bool
}

// MemoryUsage return the memory usage of NominalSort
func (ns *NominalSort) MemoryUsage() (sum int64) {
	if ns == nil {
		return
	}

	sum = ns.basePhysicalPlan.MemoryUsage() + size.SizeOfSlice + int64(cap(ns.ByItems))*size.SizeOfPointer +
		size.SizeOfBool
	for _, byItem := range ns.ByItems {
		sum += byItem.MemoryUsage()
	}
	return
}

// PhysicalUnionScan represents a union scan operator.
type PhysicalUnionScan struct {
	basePhysicalPlan

	Conditions []expression.Expression

	HandleCols HandleCols
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalUnionScan) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0)
	for _, cond := range p.Conditions {
		corCols = append(corCols, expression.ExtractCorColumns(cond)...)
	}
	return corCols
}

// MemoryUsage return the memory usage of PhysicalUnionScan
func (p *PhysicalUnionScan) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalPlan.MemoryUsage() + size.SizeOfSlice
	if p.HandleCols != nil {
		sum += p.HandleCols.MemoryUsage()
	}
	for _, cond := range p.Conditions {
		sum += cond.MemoryUsage()
	}
	return
}

// IsPartition returns true and partition ID if it works on a partition.
func (p *PhysicalIndexScan) IsPartition() (bool, int64) {
	return p.isPartition, p.physicalTableID
}

// IsPointGetByUniqueKey checks whether is a point get by unique key.
func (p *PhysicalIndexScan) IsPointGetByUniqueKey(sctx sessionctx.Context) bool {
	return len(p.Ranges) == 1 &&
		p.Index.Unique &&
		len(p.Ranges[0].LowVal) == len(p.Index.Columns) &&
		p.Ranges[0].IsPointNonNullable(sctx)
}

// PhysicalSelection represents a filter.
type PhysicalSelection struct {
	basePhysicalPlan

	Conditions []expression.Expression

	// The flag indicates whether this Selection is from a DataSource.
	// The flag is only used by cost model for compatibility and will be removed later.
	// Please see https://github.com/pingcap/tidb/issues/36243 for more details.
	fromDataSource bool

	// todo Since the feature of adding filter operators has not yet been implemented,
	// the following code for this function will not be used for now.
	// The flag indicates whether this Selection is used for RuntimeFilter
	// True: Used for RuntimeFilter
	// False: Only for normal conditions
	// hasRFConditions bool
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalSelection) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalSelection)
	base, err := p.basePhysicalPlan.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalPlan = *base
	cloned.Conditions = util.CloneExprs(p.Conditions)
	return cloned, nil
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalSelection) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(p.Conditions))
	for _, cond := range p.Conditions {
		corCols = append(corCols, expression.ExtractCorColumns(cond)...)
	}
	return corCols
}

// MemoryUsage return the memory usage of PhysicalSelection
func (p *PhysicalSelection) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalPlan.MemoryUsage() + size.SizeOfBool
	for _, expr := range p.Conditions {
		sum += expr.MemoryUsage()
	}
	return
}

// PhysicalMaxOneRow is the physical operator of maxOneRow.
type PhysicalMaxOneRow struct {
	basePhysicalPlan
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalMaxOneRow) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalMaxOneRow)
	base, err := p.basePhysicalPlan.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.basePhysicalPlan = *base
	return cloned, nil
}

// MemoryUsage return the memory usage of PhysicalMaxOneRow
func (p *PhysicalMaxOneRow) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	return p.basePhysicalPlan.MemoryUsage()
}

// PhysicalTableDual is the physical operator of dual.
type PhysicalTableDual struct {
	physicalSchemaProducer

	RowCount int

	// names is used for OutputNames() method. Dual may be inited when building point get plan.
	// So it needs to hold names for itself.
	names []*types.FieldName
}

// OutputNames returns the outputting names of each column.
func (p *PhysicalTableDual) OutputNames() types.NameSlice {
	return p.names
}

// SetOutputNames sets the outputting name by the given slice.
func (p *PhysicalTableDual) SetOutputNames(names types.NameSlice) {
	p.names = names
}

// MemoryUsage return the memory usage of PhysicalTableDual
func (p *PhysicalTableDual) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfInt + size.SizeOfSlice + int64(cap(p.names))*size.SizeOfPointer
	for _, name := range p.names {
		sum += name.MemoryUsage()
	}
	return
}

// PhysicalWindow is the physical operator of window function.
type PhysicalWindow struct {
	physicalSchemaProducer

	WindowFuncDescs []*aggregation.WindowFuncDesc
	PartitionBy     []property.SortItem
	OrderBy         []property.SortItem
	Frame           *WindowFrame

	// on which store the window function executes.
	storeTp kv.StoreType
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalWindow) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := make([]*expression.CorrelatedColumn, 0, len(p.WindowFuncDescs))
	for _, windowFunc := range p.WindowFuncDescs {
		for _, arg := range windowFunc.Args {
			corCols = append(corCols, expression.ExtractCorColumns(arg)...)
		}
	}
	if p.Frame != nil {
		if p.Frame.Start != nil {
			for _, expr := range p.Frame.Start.CalcFuncs {
				corCols = append(corCols, expression.ExtractCorColumns(expr)...)
			}
		}
		if p.Frame.End != nil {
			for _, expr := range p.Frame.End.CalcFuncs {
				corCols = append(corCols, expression.ExtractCorColumns(expr)...)
			}
		}
	}
	return corCols
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalWindow) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalWindow)
	*cloned = *p
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	cloned.PartitionBy = make([]property.SortItem, 0, len(p.PartitionBy))
	for _, it := range p.PartitionBy {
		cloned.PartitionBy = append(cloned.PartitionBy, it.Clone())
	}
	cloned.OrderBy = make([]property.SortItem, 0, len(p.OrderBy))
	for _, it := range p.OrderBy {
		cloned.OrderBy = append(cloned.OrderBy, it.Clone())
	}
	cloned.WindowFuncDescs = make([]*aggregation.WindowFuncDesc, 0, len(p.WindowFuncDescs))
	for _, it := range p.WindowFuncDescs {
		cloned.WindowFuncDescs = append(cloned.WindowFuncDescs, it.Clone())
	}
	if p.Frame != nil {
		cloned.Frame = p.Frame.Clone()
	}

	return cloned, nil
}

// MemoryUsage return the memory usage of PhysicalWindow
func (p *PhysicalWindow) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfSlice*3 + int64(cap(p.WindowFuncDescs))*size.SizeOfPointer +
		size.SizeOfUint8

	for _, windowFunc := range p.WindowFuncDescs {
		sum += windowFunc.MemoryUsage()
	}
	for _, item := range p.PartitionBy {
		sum += item.MemoryUsage()
	}
	for _, item := range p.OrderBy {
		sum += item.MemoryUsage()
	}
	return
}

// PhysicalShuffle represents a shuffle plan.
// `Tails` and `DataSources` are the last plan within and the first plan following the "shuffle", respectively,
//
//	to build the child executors chain.
//
// Take `Window` operator for example:
//
//	Shuffle -> Window -> Sort -> DataSource, will be separated into:
//	  ==> Shuffle: for main thread
//	  ==> Window -> Sort(:Tail) -> shuffleWorker: for workers
//	  ==> DataSource: for `fetchDataAndSplit` thread
type PhysicalShuffle struct {
	basePhysicalPlan

	Concurrency int
	Tails       []PhysicalPlan
	DataSources []PhysicalPlan

	SplitterType PartitionSplitterType
	ByItemArrays [][]expression.Expression
}

// MemoryUsage return the memory usage of PhysicalShuffle
func (p *PhysicalShuffle) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.basePhysicalPlan.MemoryUsage() + size.SizeOfInt*2 + size.SizeOfSlice*(3+int64(cap(p.ByItemArrays))) +
		int64(cap(p.Tails)+cap(p.DataSources))*size.SizeOfInterface

	for _, plan := range p.Tails {
		sum += plan.MemoryUsage()
	}
	for _, plan := range p.DataSources {
		sum += plan.MemoryUsage()
	}
	for _, exprs := range p.ByItemArrays {
		sum += int64(cap(exprs)) * size.SizeOfInterface
		for _, expr := range exprs {
			sum += expr.MemoryUsage()
		}
	}
	return
}

// PartitionSplitterType is the type of `Shuffle` executor splitter, which splits data source into partitions.
type PartitionSplitterType int

const (
	// PartitionHashSplitterType is the splitter splits by hash.
	PartitionHashSplitterType = iota
	// PartitionRangeSplitterType is the splitter that split sorted data into the same range
	PartitionRangeSplitterType
)

// PhysicalShuffleReceiverStub represents a receiver stub of `PhysicalShuffle`,
// and actually, is executed by `executor.shuffleWorker`.
type PhysicalShuffleReceiverStub struct {
	physicalSchemaProducer

	// Receiver points to `executor.shuffleReceiver`.
	Receiver unsafe.Pointer
	// DataSource is the PhysicalPlan of the Receiver.
	DataSource PhysicalPlan
}

// MemoryUsage return the memory usage of PhysicalShuffleReceiverStub
func (p *PhysicalShuffleReceiverStub) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + size.SizeOfPointer + size.SizeOfInterface
	if p.DataSource != nil {
		sum += p.DataSource.MemoryUsage()
	}
	return
}

// CollectPlanStatsVersion uses to collect the statistics version of the plan.
func CollectPlanStatsVersion(plan PhysicalPlan, statsInfos map[string]uint64) map[string]uint64 {
	for _, child := range plan.Children() {
		statsInfos = CollectPlanStatsVersion(child, statsInfos)
	}
	switch copPlan := plan.(type) {
	case *PhysicalTableReader:
		statsInfos = CollectPlanStatsVersion(copPlan.tablePlan, statsInfos)
	case *PhysicalIndexReader:
		statsInfos = CollectPlanStatsVersion(copPlan.indexPlan, statsInfos)
	case *PhysicalIndexLookUpReader:
		// For index loop up, only the indexPlan is necessary,
		// because they use the same stats and we do not set the stats info for tablePlan.
		statsInfos = CollectPlanStatsVersion(copPlan.indexPlan, statsInfos)
	case *PhysicalIndexScan:
		statsInfos[copPlan.Table.Name.O] = copPlan.stats.StatsVersion
	case *PhysicalTableScan:
		statsInfos[copPlan.Table.Name.O] = copPlan.stats.StatsVersion
	}

	return statsInfos
}

// PhysicalShow represents a show plan.
type PhysicalShow struct {
	physicalSchemaProducer

	ShowContents

	Extractor ShowPredicateExtractor
}

// MemoryUsage return the memory usage of PhysicalShow
func (p *PhysicalShow) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + p.ShowContents.MemoryUsage() + size.SizeOfInterface
	return
}

// PhysicalShowDDLJobs is for showing DDL job list.
type PhysicalShowDDLJobs struct {
	physicalSchemaProducer

	JobNumber int64
}

// MemoryUsage return the memory usage of PhysicalShowDDLJobs
func (p *PhysicalShowDDLJobs) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}
	return p.physicalSchemaProducer.MemoryUsage() + size.SizeOfInt64
}

// BuildMergeJoinPlan builds a PhysicalMergeJoin from the given fields. Currently, it is only used for test purpose.
func BuildMergeJoinPlan(ctx sessionctx.Context, joinType JoinType, leftKeys, rightKeys []*expression.Column) *PhysicalMergeJoin {
	baseJoin := basePhysicalJoin{
		JoinType:      joinType,
		DefaultValues: []types.Datum{types.NewDatum(1), types.NewDatum(1)},
		LeftJoinKeys:  leftKeys,
		RightJoinKeys: rightKeys,
	}
	return PhysicalMergeJoin{basePhysicalJoin: baseJoin}.Init(ctx, nil, 0)
}

// SafeClone clones this PhysicalPlan and handles its panic.
func SafeClone(v PhysicalPlan) (_ PhysicalPlan, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Errorf("%v", r)
		}
	}()
	return v.Clone()
}

// PhysicalTableSample represents a table sample plan.
// It returns the sample rows to its parent operand.
type PhysicalTableSample struct {
	physicalSchemaProducer
	TableSampleInfo *TableSampleInfo
	TableInfo       table.Table
	Desc            bool
}

// TableSampleInfo contains the information for PhysicalTableSample.
type TableSampleInfo struct {
	AstNode    *ast.TableSample
	FullSchema *expression.Schema
	Partitions []table.PartitionedTable
}

// MemoryUsage return the memory usage of TableSampleInfo
func (t *TableSampleInfo) MemoryUsage() (sum int64) {
	if t == nil {
		return
	}

	sum = size.SizeOfPointer*2 + size.SizeOfSlice + int64(cap(t.Partitions))*size.SizeOfInterface
	if t.AstNode != nil {
		sum += int64(unsafe.Sizeof(ast.TableSample{}))
	}
	if t.FullSchema != nil {
		sum += t.FullSchema.MemoryUsage()
	}
	return
}

// NewTableSampleInfo creates a new TableSampleInfo.
func NewTableSampleInfo(node *ast.TableSample, fullSchema *expression.Schema, pt []table.PartitionedTable) *TableSampleInfo {
	if node == nil {
		return nil
	}
	return &TableSampleInfo{
		AstNode:    node,
		FullSchema: fullSchema,
		Partitions: pt,
	}
}

// PhysicalCTE is for CTE.
type PhysicalCTE struct {
	physicalSchemaProducer

	SeedPlan  PhysicalPlan
	RecurPlan PhysicalPlan
	CTE       *CTEClass
	cteAsName model.CIStr
	cteName   model.CIStr

	readerReceiver *PhysicalExchangeReceiver
	storageSender  *PhysicalExchangeSender
}

// PhysicalCTETable is for CTE table.
type PhysicalCTETable struct {
	physicalSchemaProducer

	IDForStorage int
}

// ExtractCorrelatedCols implements PhysicalPlan interface.
func (p *PhysicalCTE) ExtractCorrelatedCols() []*expression.CorrelatedColumn {
	corCols := ExtractCorrelatedCols4PhysicalPlan(p.SeedPlan)
	if p.RecurPlan != nil {
		corCols = append(corCols, ExtractCorrelatedCols4PhysicalPlan(p.RecurPlan)...)
	}
	return corCols
}

// OperatorInfo implements dataAccesser interface.
func (p *PhysicalCTE) OperatorInfo(_ bool) string {
	return fmt.Sprintf("data:%s", (*CTEDefinition)(p).ExplainID())
}

// ExplainInfo implements Plan interface.
func (p *PhysicalCTE) ExplainInfo() string {
	return p.AccessObject().String() + ", " + p.OperatorInfo(false)
}

// ExplainID overrides the ExplainID.
func (p *PhysicalCTE) ExplainID() fmt.Stringer {
	return stringutil.MemoizeStr(func() string {
		if p.ctx != nil && p.ctx.GetSessionVars().StmtCtx.IgnoreExplainIDSuffix {
			return p.TP()
		}
		return p.TP() + "_" + strconv.Itoa(p.id)
	})
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalCTE) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalCTE)
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	if p.SeedPlan != nil {
		cloned.SeedPlan, err = p.SeedPlan.Clone()
		if err != nil {
			return nil, err
		}
	}
	if p.RecurPlan != nil {
		cloned.RecurPlan, err = p.RecurPlan.Clone()
		if err != nil {
			return nil, err
		}
	}
	cloned.cteAsName, cloned.cteName = p.cteAsName, p.cteName
	cloned.CTE = p.CTE
	if p.storageSender != nil {
		clonedSender, err := p.storageSender.Clone()
		if err != nil {
			return nil, err
		}
		cloned.storageSender = clonedSender.(*PhysicalExchangeSender)
	}
	if p.readerReceiver != nil {
		clonedReceiver, err := p.readerReceiver.Clone()
		if err != nil {
			return nil, err
		}
		cloned.readerReceiver = clonedReceiver.(*PhysicalExchangeReceiver)
	}
	return cloned, nil
}

// MemoryUsage return the memory usage of PhysicalCTE
func (p *PhysicalCTE) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + p.cteAsName.MemoryUsage()
	if p.SeedPlan != nil {
		sum += p.SeedPlan.MemoryUsage()
	}
	if p.RecurPlan != nil {
		sum += p.RecurPlan.MemoryUsage()
	}
	if p.CTE != nil {
		sum += p.CTE.MemoryUsage()
	}
	return
}

// ExplainInfo overrides the ExplainInfo
func (p *PhysicalCTETable) ExplainInfo() string {
	return "Scan on CTE_" + strconv.Itoa(p.IDForStorage)
}

// MemoryUsage return the memory usage of PhysicalCTETable
func (p *PhysicalCTETable) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	return p.physicalSchemaProducer.MemoryUsage() + size.SizeOfInt
}

// CTEDefinition is CTE definition for explain.
type CTEDefinition PhysicalCTE

// ExplainInfo overrides the ExplainInfo
func (p *CTEDefinition) ExplainInfo() string {
	var res string
	if p.RecurPlan != nil {
		res = "Recursive CTE"
	} else {
		res = "Non-Recursive CTE"
	}
	if p.CTE.HasLimit {
		res += fmt.Sprintf(", limit(offset:%v, count:%v)", p.CTE.LimitBeg, p.CTE.LimitEnd-p.CTE.LimitBeg)
	}
	return res
}

// ExplainID overrides the ExplainID.
func (p *CTEDefinition) ExplainID() fmt.Stringer {
	return stringutil.MemoizeStr(func() string {
		return "CTE_" + strconv.Itoa(p.CTE.IDForStorage)
	})
}

// MemoryUsage return the memory usage of CTEDefinition
func (p *CTEDefinition) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + p.cteAsName.MemoryUsage()
	if p.SeedPlan != nil {
		sum += p.SeedPlan.MemoryUsage()
	}
	if p.RecurPlan != nil {
		sum += p.RecurPlan.MemoryUsage()
	}
	if p.CTE != nil {
		sum += p.CTE.MemoryUsage()
	}
	return
}

// PhysicalCTEStorage is used for representing CTE storage, or CTE producer in other words.
type PhysicalCTEStorage PhysicalCTE

// ExplainInfo overrides the ExplainInfo
func (*PhysicalCTEStorage) ExplainInfo() string {
	return "Non-Recursive CTE Storage"
}

// ExplainID overrides the ExplainID.
func (p *PhysicalCTEStorage) ExplainID() fmt.Stringer {
	return stringutil.MemoizeStr(func() string {
		return "CTE_" + strconv.Itoa(p.CTE.IDForStorage)
	})
}

// MemoryUsage return the memory usage of CTEDefinition
func (p *PhysicalCTEStorage) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage() + p.cteAsName.MemoryUsage()
	if p.CTE != nil {
		sum += p.CTE.MemoryUsage()
	}
	return
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalCTEStorage) Clone() (PhysicalPlan, error) {
	cloned, err := (*PhysicalCTE)(p).Clone()
	if err != nil {
		return nil, err
	}
	return (*PhysicalCTEStorage)(cloned.(*PhysicalCTE)), nil
}

func appendChildCandidate(origin PhysicalPlan, pp PhysicalPlan, op *physicalOptimizeOp) {
	candidate := &tracing.CandidatePlanTrace{
		PlanTrace: &tracing.PlanTrace{
			ID:          pp.ID(),
			TP:          pp.TP(),
			ExplainInfo: pp.ExplainInfo(),
			// TODO: trace the cost
		},
	}
	op.tracer.AppendCandidate(candidate)
	pp.appendChildCandidate(op)
	op.tracer.Candidates[origin.ID()].AppendChildrenID(pp.ID())
}

// PhysicalSequence is the physical representation of LogicalSequence. Used to mark the CTE producers in the plan tree.
type PhysicalSequence struct {
	physicalSchemaProducer
}

// MemoryUsage returns the memory usage of the PhysicalSequence.
func (p *PhysicalSequence) MemoryUsage() (sum int64) {
	if p == nil {
		return
	}

	sum = p.physicalSchemaProducer.MemoryUsage()

	return
}

// ExplainID overrides the ExplainID.
func (p *PhysicalSequence) ExplainID() fmt.Stringer {
	return stringutil.MemoizeStr(func() string {
		if p.ctx != nil && p.ctx.GetSessionVars().StmtCtx.IgnoreExplainIDSuffix {
			return p.TP()
		}
		return p.TP() + "_" + strconv.Itoa(p.id)
	})
}

// ExplainInfo overrides the ExplainInfo.
func (*PhysicalSequence) ExplainInfo() string {
	res := "Sequence Node"
	return res
}

// Clone implements PhysicalPlan interface.
func (p *PhysicalSequence) Clone() (PhysicalPlan, error) {
	cloned := new(PhysicalSequence)
	base, err := p.physicalSchemaProducer.cloneWithSelf(cloned)
	if err != nil {
		return nil, err
	}
	cloned.physicalSchemaProducer = *base
	return cloned, nil
}

// Schema returns its last child(which is the main query tree)'s schema.
func (p *PhysicalSequence) Schema() *expression.Schema {
	return p.Children()[len(p.Children())-1].Schema()
}
