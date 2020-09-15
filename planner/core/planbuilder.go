// Copyright 2015 PingCAP, Inc.
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

package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/charset"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/opcode"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/planner/property"
	"github.com/pingcap/tidb/planner/util"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	driver "github.com/pingcap/tidb/types/parser_driver"
	util2 "github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/hint"
	"github.com/pingcap/tidb/util/logutil"
	utilparser "github.com/pingcap/tidb/util/parser"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tidb/util/set"

	"github.com/cznic/mathutil"
	"github.com/pingcap/tidb/table/tables"
	"go.uber.org/zap"
)

type visitInfo struct {
	privilege mysql.PrivilegeType
	db        string
	table     string
	column    string
	err       error
}

type indexNestedLoopJoinTables struct {
	inljTables  []hintTableInfo
	inlhjTables []hintTableInfo
	inlmjTables []hintTableInfo
}

type tableHintInfo struct {
	indexNestedLoopJoinTables
	sortMergeJoinTables         []hintTableInfo
	broadcastJoinTables         []hintTableInfo
	broadcastJoinPreferredLocal []hintTableInfo
	hashJoinTables              []hintTableInfo
	indexHintList               []indexHintInfo
	tiflashTables               []hintTableInfo
	tikvTables                  []hintTableInfo
	aggHints                    aggHintInfo
	indexMergeHintList          []indexHintInfo
	timeRangeHint               ast.HintTimeRange
	topnHints                   topnHintInfo
}

type topnHintInfo struct {
	preferTopNToCop bool
}

type hintTableInfo struct {
	dbName       model.CIStr
	tblName      model.CIStr
	partitions   []model.CIStr
	selectOffset int
	matched      bool
}

type indexHintInfo struct {
	dbName     model.CIStr
	tblName    model.CIStr
	partitions []model.CIStr
	indexHint  *ast.IndexHint
	// Matched indicates whether this index hint
	// has been successfully applied to a DataSource.
	// If an indexHintInfo is not matched after building
	// a Select statement, we will generate a warning for it.
	matched bool
}

func (hint *indexHintInfo) hintTypeString() string {
	switch hint.indexHint.HintType {
	case ast.HintUse:
		return "use_index"
	case ast.HintIgnore:
		return "ignore_index"
	case ast.HintForce:
		return "force_index"
	}
	return ""
}

// indexString formats the indexHint as dbName.tableName[, indexNames].
func (hint *indexHintInfo) indexString() string {
	var indexListString string
	indexList := make([]string, len(hint.indexHint.IndexNames))
	for i := range hint.indexHint.IndexNames {
		indexList[i] = hint.indexHint.IndexNames[i].L
	}
	if len(indexList) > 0 {
		indexListString = fmt.Sprintf(", %s", strings.Join(indexList, ", "))
	}
	return fmt.Sprintf("%s.%s%s", hint.dbName, hint.tblName, indexListString)
}

type aggHintInfo struct {
	preferAggType  uint
	preferAggToCop bool
}

// QueryTimeRange represents a time range specified by TIME_RANGE hint
type QueryTimeRange struct {
	From time.Time
	To   time.Time
}

// Condition returns a WHERE clause base on it's value
func (tr *QueryTimeRange) Condition() string {
	return fmt.Sprintf("where time>='%s' and time<='%s'", tr.From.Format(MetricTableTimeFormat), tr.To.Format(MetricTableTimeFormat))
}

func tableNames2HintTableInfo(ctx sessionctx.Context, hintName string, hintTables []ast.HintTable, p *hint.BlockHintProcessor, nodeType hint.NodeType, currentOffset int) []hintTableInfo {
	if len(hintTables) == 0 {
		return nil
	}
	hintTableInfos := make([]hintTableInfo, 0, len(hintTables))
	defaultDBName := model.NewCIStr(ctx.GetSessionVars().CurrentDB)
	isInapplicable := false
	for _, hintTable := range hintTables {
		tableInfo := hintTableInfo{
			dbName:       hintTable.DBName,
			tblName:      hintTable.TableName,
			partitions:   hintTable.PartitionList,
			selectOffset: p.GetHintOffset(hintTable.QBName, nodeType, currentOffset),
		}
		if tableInfo.dbName.L == "" {
			tableInfo.dbName = defaultDBName
		}
		switch hintName {
		case TiDBMergeJoin, HintSMJ, TiDBIndexNestedLoopJoin, HintINLJ, HintINLHJ, HintINLMJ, TiDBHashJoin, HintHJ:
			if len(tableInfo.partitions) > 0 {
				isInapplicable = true
			}
		}
		hintTableInfos = append(hintTableInfos, tableInfo)
	}
	if isInapplicable {
		ctx.GetSessionVars().StmtCtx.AppendWarning(
			errors.New(fmt.Sprintf("Optimizer Hint %s is inapplicable on specified partitions",
				restore2JoinHint(hintName, hintTableInfos))))
		return nil
	}
	return hintTableInfos
}

// ifPreferAsLocalInBCJoin checks if there is a data source specified as local read by hint
func (info *tableHintInfo) ifPreferAsLocalInBCJoin(p LogicalPlan, blockOffset int) bool {
	alias := extractTableAlias(p, blockOffset)
	if alias != nil {
		tableNames := make([]*hintTableInfo, 1)
		tableNames[0] = alias
		return info.matchTableName(tableNames, info.broadcastJoinPreferredLocal)
	}
	for _, c := range p.Children() {
		if info.ifPreferAsLocalInBCJoin(c, blockOffset) {
			return true
		}
	}
	return false
}

func (info *tableHintInfo) ifPreferMergeJoin(tableNames ...*hintTableInfo) bool {
	return info.matchTableName(tableNames, info.sortMergeJoinTables)
}

func (info *tableHintInfo) ifPreferBroadcastJoin(tableNames ...*hintTableInfo) bool {
	return info.matchTableName(tableNames, info.broadcastJoinTables)
}

func (info *tableHintInfo) ifPreferHashJoin(tableNames ...*hintTableInfo) bool {
	return info.matchTableName(tableNames, info.hashJoinTables)
}

func (info *tableHintInfo) ifPreferINLJ(tableNames ...*hintTableInfo) bool {
	return info.matchTableName(tableNames, info.indexNestedLoopJoinTables.inljTables)
}

func (info *tableHintInfo) ifPreferINLHJ(tableNames ...*hintTableInfo) bool {
	return info.matchTableName(tableNames, info.indexNestedLoopJoinTables.inlhjTables)
}

func (info *tableHintInfo) ifPreferINLMJ(tableNames ...*hintTableInfo) bool {
	return info.matchTableName(tableNames, info.indexNestedLoopJoinTables.inlmjTables)
}

func (info *tableHintInfo) ifPreferTiFlash(tableName *hintTableInfo) *hintTableInfo {
	if tableName == nil {
		return nil
	}
	for i, tbl := range info.tiflashTables {
		if tableName.dbName.L == tbl.dbName.L && tableName.tblName.L == tbl.tblName.L && tbl.selectOffset == tableName.selectOffset {
			info.tiflashTables[i].matched = true
			return &tbl
		}
	}
	return nil
}

func (info *tableHintInfo) ifPreferTiKV(tableName *hintTableInfo) *hintTableInfo {
	if tableName == nil {
		return nil
	}
	for i, tbl := range info.tikvTables {
		if tableName.dbName.L == tbl.dbName.L && tableName.tblName.L == tbl.tblName.L && tbl.selectOffset == tableName.selectOffset {
			info.tikvTables[i].matched = true
			return &tbl
		}
	}
	return nil
}

// matchTableName checks whether the hint hit the need.
// Only need either side matches one on the list.
// Even though you can put 2 tables on the list,
// it doesn't mean optimizer will reorder to make them
// join directly.
// Which it joins on with depend on sequence of traverse
// and without reorder, user might adjust themselves.
// This is similar to MySQL hints.
func (info *tableHintInfo) matchTableName(tables []*hintTableInfo, hintTables []hintTableInfo) bool {
	hintMatched := false
	for _, table := range tables {
		for i, curEntry := range hintTables {
			if table == nil {
				continue
			}
			if curEntry.dbName.L == table.dbName.L && curEntry.tblName.L == table.tblName.L && table.selectOffset == curEntry.selectOffset {
				hintTables[i].matched = true
				hintMatched = true
				break
			}
		}
	}
	return hintMatched
}

func restore2TableHint(hintTables ...hintTableInfo) string {
	buffer := bytes.NewBufferString("")
	for i, table := range hintTables {
		buffer.WriteString(table.tblName.L)
		if len(table.partitions) > 0 {
			buffer.WriteString(" PARTITION(")
			for j, partition := range table.partitions {
				if j > 0 {
					buffer.WriteString(", ")
				}
				buffer.WriteString(partition.L)
			}
			buffer.WriteString(")")
		}
		if i < len(hintTables)-1 {
			buffer.WriteString(", ")
		}
	}
	return buffer.String()
}

func restore2JoinHint(hintType string, hintTables []hintTableInfo) string {
	buffer := bytes.NewBufferString("/*+ ")
	buffer.WriteString(strings.ToUpper(hintType))
	buffer.WriteString("(")
	buffer.WriteString(restore2TableHint(hintTables...))
	buffer.WriteString(") */")
	return buffer.String()
}

func restore2IndexHint(hintType string, hintIndex indexHintInfo) string {
	buffer := bytes.NewBufferString("/*+ ")
	buffer.WriteString(strings.ToUpper(hintType))
	buffer.WriteString("(")
	buffer.WriteString(restore2TableHint(hintTableInfo{
		dbName:     hintIndex.dbName,
		tblName:    hintIndex.tblName,
		partitions: hintIndex.partitions,
	}))
	if hintIndex.indexHint != nil && len(hintIndex.indexHint.IndexNames) > 0 {
		for i, indexName := range hintIndex.indexHint.IndexNames {
			if i > 0 {
				buffer.WriteString(",")
			}
			buffer.WriteString(" " + indexName.L)
		}
	}
	buffer.WriteString(") */")
	return buffer.String()
}

func restore2StorageHint(tiflashTables, tikvTables []hintTableInfo) string {
	buffer := bytes.NewBufferString("/*+ ")
	buffer.WriteString(strings.ToUpper(HintReadFromStorage))
	buffer.WriteString("(")
	if len(tiflashTables) > 0 {
		buffer.WriteString("tiflash[")
		buffer.WriteString(restore2TableHint(tiflashTables...))
		buffer.WriteString("]")
		if len(tikvTables) > 0 {
			buffer.WriteString(", ")
		}
	}
	if len(tikvTables) > 0 {
		buffer.WriteString("tikv[")
		buffer.WriteString(restore2TableHint(tikvTables...))
		buffer.WriteString("]")
	}
	buffer.WriteString(") */")
	return buffer.String()
}

func extractUnmatchedTables(hintTables []hintTableInfo) []string {
	var tableNames []string
	for _, table := range hintTables {
		if !table.matched {
			tableNames = append(tableNames, table.tblName.O)
		}
	}
	return tableNames
}

// clauseCode indicates in which clause the column is currently.
type clauseCode int

const (
	unknowClause clauseCode = iota
	fieldList
	havingClause
	onClause
	orderByClause
	whereClause
	groupByClause
	showStatement
	globalOrderByClause
)

var clauseMsg = map[clauseCode]string{
	unknowClause:        "",
	fieldList:           "field list",
	havingClause:        "having clause",
	onClause:            "on clause",
	orderByClause:       "order clause",
	whereClause:         "where clause",
	groupByClause:       "group statement",
	showStatement:       "show statement",
	globalOrderByClause: "global ORDER clause",
}

type capFlagType = uint64

const (
	_ capFlagType = iota
	// canExpandAST indicates whether the origin AST can be expanded during plan
	// building. ONLY used for `CreateViewStmt` now.
	canExpandAST
	// collectUnderlyingViewName indicates whether to collect the underlying
	// view names of a CreateViewStmt during plan building.
	collectUnderlyingViewName
)

// PlanBuilder builds Plan from an ast.Node.
// It just builds the ast node straightforwardly.
type PlanBuilder struct {
	ctx          sessionctx.Context
	is           infoschema.InfoSchema
	outerSchemas []*expression.Schema
	outerNames   [][]*types.FieldName
	// colMapper stores the column that must be pre-resolved.
	colMapper map[*ast.ColumnNameExpr]int
	// visitInfo is used for privilege check.
	visitInfo     []visitInfo
	tableHintInfo []tableHintInfo
	// optFlag indicates the flags of the optimizer rules.
	optFlag uint64
	// capFlag indicates the capability flags.
	capFlag capFlagType

	curClause clauseCode

	// rewriterPool stores the expressionRewriter we have created to reuse it if it has been released.
	// rewriterCounter counts how many rewriter is being used.
	rewriterPool    []*expressionRewriter
	rewriterCounter int

	windowSpecs  map[string]*ast.WindowSpec
	inUpdateStmt bool
	inDeleteStmt bool
	// inStraightJoin represents whether the current "SELECT" statement has
	// "STRAIGHT_JOIN" option.
	inStraightJoin bool

	// handleHelper records the handle column position for tables. Delete/Update/SelectLock/UnionScan may need this information.
	// It collects the information by the following procedure:
	//   Since we build the plan tree from bottom to top, we maintain a stack to record the current handle information.
	//   If it's a dataSource/tableDual node, we create a new map.
	//   If it's a aggregation, we pop the map and push a nil map since no handle information left.
	//   If it's a union, we pop all children's and push a nil map.
	//   If it's a join, we pop its children's out then merge them and push the new map to stack.
	//   If we meet a subquery, it's clearly that it's a independent problem so we just pop one map out when we finish building the subquery.
	handleHelper *handleColHelper

	hintProcessor *hint.BlockHintProcessor
	// selectOffset is the offsets of current processing select stmts.
	selectOffset []int

	// SelectLock need this information to locate the lock on partitions.
	partitionedTable []table.PartitionedTable
	// CreateView needs this information to check whether exists nested view.
	underlyingViewNames set.StringSet
}

type handleColHelper struct {
	id2HandleMapStack []map[int64][]HandleCols
	stackTail         int
}

func (hch *handleColHelper) appendColToLastMap(tblID int64, handleCols HandleCols) {
	tailMap := hch.id2HandleMapStack[hch.stackTail-1]
	tailMap[tblID] = append(tailMap[tblID], handleCols)
}

func (hch *handleColHelper) popMap() map[int64][]HandleCols {
	ret := hch.id2HandleMapStack[hch.stackTail-1]
	hch.stackTail--
	hch.id2HandleMapStack = hch.id2HandleMapStack[:hch.stackTail]
	return ret
}

func (hch *handleColHelper) pushMap(m map[int64][]HandleCols) {
	hch.id2HandleMapStack = append(hch.id2HandleMapStack, m)
	hch.stackTail++
}

func (hch *handleColHelper) mergeAndPush(m1, m2 map[int64][]HandleCols) {
	newMap := make(map[int64][]HandleCols, mathutil.Max(len(m1), len(m2)))
	for k, v := range m1 {
		newMap[k] = make([]HandleCols, len(v))
		copy(newMap[k], v)
	}
	for k, v := range m2 {
		if _, ok := newMap[k]; ok {
			newMap[k] = append(newMap[k], v...)
		} else {
			newMap[k] = make([]HandleCols, len(v))
			copy(newMap[k], v)
		}
	}
	hch.pushMap(newMap)
}

func (hch *handleColHelper) tailMap() map[int64][]HandleCols {
	return hch.id2HandleMapStack[hch.stackTail-1]
}

// GetVisitInfo gets the visitInfo of the PlanBuilder.
func (b *PlanBuilder) GetVisitInfo() []visitInfo {
	return b.visitInfo
}

// GetDBTableInfo gets the accessed dbs and tables info.
func (b *PlanBuilder) GetDBTableInfo() []stmtctx.TableEntry {
	var tables []stmtctx.TableEntry
	existsFunc := func(tbls []stmtctx.TableEntry, tbl *stmtctx.TableEntry) bool {
		for _, t := range tbls {
			if t == *tbl {
				return true
			}
		}
		return false
	}
	for _, v := range b.visitInfo {
		tbl := &stmtctx.TableEntry{DB: v.db, Table: v.table}
		if !existsFunc(tables, tbl) {
			tables = append(tables, *tbl)
		}
	}
	return tables
}

// GetOptFlag gets the optFlag of the PlanBuilder.
func (b *PlanBuilder) GetOptFlag() uint64 {
	return b.optFlag
}

func (b *PlanBuilder) getSelectOffset() int {
	if len(b.selectOffset) > 0 {
		return b.selectOffset[len(b.selectOffset)-1]
	}
	return -1
}

func (b *PlanBuilder) pushSelectOffset(offset int) {
	b.selectOffset = append(b.selectOffset, offset)
}

func (b *PlanBuilder) popSelectOffset() {
	b.selectOffset = b.selectOffset[:len(b.selectOffset)-1]
}

// NewPlanBuilder creates a new PlanBuilder.
func NewPlanBuilder(sctx sessionctx.Context, is infoschema.InfoSchema, processor *hint.BlockHintProcessor) *PlanBuilder {
	if processor == nil {
		sctx.GetSessionVars().PlannerSelectBlockAsName = nil
	} else {
		sctx.GetSessionVars().PlannerSelectBlockAsName = make([]ast.HintTable, processor.MaxSelectStmtOffset()+1)
	}
	return &PlanBuilder{
		ctx:           sctx,
		is:            is,
		colMapper:     make(map[*ast.ColumnNameExpr]int),
		handleHelper:  &handleColHelper{id2HandleMapStack: make([]map[int64][]HandleCols, 0)},
		hintProcessor: processor,
	}
}

// Build builds the ast node to a Plan.
func (b *PlanBuilder) Build(ctx context.Context, node ast.Node) (Plan, error) {
	b.optFlag |= flagPrunColumns
	switch x := node.(type) {
	case *ast.AdminStmt:
		return b.buildAdmin(ctx, x)
	case *ast.DeallocateStmt:
		return &Deallocate{Name: x.Name}, nil
	case *ast.DeleteStmt:
		return b.buildDelete(ctx, x)
	case *ast.ExecuteStmt:
		return b.buildExecute(ctx, x)
	case *ast.ExplainStmt:
		return b.buildExplain(ctx, x)
	case *ast.ExplainForStmt:
		return b.buildExplainFor(x)
	case *ast.TraceStmt:
		return b.buildTrace(x)
	case *ast.InsertStmt:
		return b.buildInsert(ctx, x)
	case *ast.LoadDataStmt:
		return b.buildLoadData(ctx, x)
	case *ast.LoadStatsStmt:
		return b.buildLoadStats(x), nil
	case *ast.IndexAdviseStmt:
		return b.buildIndexAdvise(x), nil
	case *ast.PrepareStmt:
		return b.buildPrepare(x), nil
	case *ast.SelectStmt:
		if x.SelectIntoOpt != nil {
			return b.buildSelectInto(ctx, x)
		}
		return b.buildSelect(ctx, x)
	case *ast.SetOprStmt:
		return b.buildSetOpr(ctx, x)
	case *ast.UpdateStmt:
		return b.buildUpdate(ctx, x)
	case *ast.ShowStmt:
		return b.buildShow(ctx, x)
	case *ast.DoStmt:
		return b.buildDo(ctx, x)
	case *ast.SetStmt:
		return b.buildSet(ctx, x)
	case *ast.SetConfigStmt:
		return b.buildSetConfig(ctx, x)
	case *ast.AnalyzeTableStmt:
		return b.buildAnalyze(x)
	case *ast.BinlogStmt, *ast.FlushStmt, *ast.UseStmt, *ast.BRIEStmt,
		*ast.BeginStmt, *ast.CommitStmt, *ast.RollbackStmt, *ast.CreateUserStmt, *ast.SetPwdStmt, *ast.AlterInstanceStmt,
		*ast.GrantStmt, *ast.DropUserStmt, *ast.AlterUserStmt, *ast.RevokeStmt, *ast.KillStmt, *ast.DropStatsStmt,
		*ast.GrantRoleStmt, *ast.RevokeRoleStmt, *ast.SetRoleStmt, *ast.SetDefaultRoleStmt, *ast.ShutdownStmt,
		*ast.CreateStatisticsStmt, *ast.DropStatisticsStmt:
		return b.buildSimple(node.(ast.StmtNode))
	case ast.DDLNode:
		return b.buildDDL(ctx, x)
	case *ast.CreateBindingStmt:
		return b.buildCreateBindPlan(x)
	case *ast.DropBindingStmt:
		return b.buildDropBindPlan(x)
	case *ast.ChangeStmt:
		return b.buildChange(x)
	case *ast.SplitRegionStmt:
		return b.buildSplitRegion(x)
	}
	return nil, ErrUnsupportedType.GenWithStack("Unsupported type %T", node)
}

func (b *PlanBuilder) buildSetConfig(ctx context.Context, v *ast.SetConfigStmt) (Plan, error) {
	privErr := ErrSpecificAccessDenied.GenWithStackByArgs("CONFIG")
	b.visitInfo = appendVisitInfo(b.visitInfo, mysql.ConfigPriv, "", "", "", privErr)
	mockTablePlan := LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
	expr, _, err := b.rewrite(ctx, v.Value, mockTablePlan, nil, true)
	return &SetConfig{Name: v.Name, Type: v.Type, Instance: v.Instance, Value: expr}, err
}

func (b *PlanBuilder) buildChange(v *ast.ChangeStmt) (Plan, error) {
	exe := &Change{
		ChangeStmt: v,
	}
	return exe, nil
}

func (b *PlanBuilder) buildExecute(ctx context.Context, v *ast.ExecuteStmt) (Plan, error) {
	vars := make([]expression.Expression, 0, len(v.UsingVars))
	for _, expr := range v.UsingVars {
		newExpr, _, err := b.rewrite(ctx, expr, nil, nil, true)
		if err != nil {
			return nil, err
		}
		vars = append(vars, newExpr)
	}
	exe := &Execute{Name: v.Name, UsingVars: vars, ExecID: v.ExecID}
	if v.BinaryArgs != nil {
		exe.PrepareParams = v.BinaryArgs.([]types.Datum)
	}
	return exe, nil
}

func (b *PlanBuilder) buildDo(ctx context.Context, v *ast.DoStmt) (Plan, error) {
	var p LogicalPlan
	dual := LogicalTableDual{RowCount: 1}.Init(b.ctx, b.getSelectOffset())
	dual.SetSchema(expression.NewSchema())
	p = dual
	proj := LogicalProjection{Exprs: make([]expression.Expression, 0, len(v.Exprs))}.Init(b.ctx, b.getSelectOffset())
	proj.names = make([]*types.FieldName, len(v.Exprs))
	schema := expression.NewSchema(make([]*expression.Column, 0, len(v.Exprs))...)
	for _, astExpr := range v.Exprs {
		expr, np, err := b.rewrite(ctx, astExpr, p, nil, true)
		if err != nil {
			return nil, err
		}
		p = np
		proj.Exprs = append(proj.Exprs, expr)
		schema.Append(&expression.Column{
			UniqueID: b.ctx.GetSessionVars().AllocPlanColumnID(),
			RetType:  expr.GetType(),
		})
	}
	proj.SetChildren(p)
	proj.self = proj
	proj.SetSchema(schema)
	proj.CalculateNoDelay = true
	return proj, nil
}

func (b *PlanBuilder) buildSet(ctx context.Context, v *ast.SetStmt) (Plan, error) {
	p := &Set{}
	for _, vars := range v.Variables {
		if vars.IsGlobal {
			err := ErrSpecificAccessDenied.GenWithStackByArgs("SUPER")
			b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", err)
		}
		assign := &expression.VarAssignment{
			Name:     vars.Name,
			IsGlobal: vars.IsGlobal,
			IsSystem: vars.IsSystem,
		}
		if _, ok := vars.Value.(*ast.DefaultExpr); !ok {
			if cn, ok2 := vars.Value.(*ast.ColumnNameExpr); ok2 && cn.Name.Table.L == "" {
				// Convert column name expression to string value expression.
				char, col := b.ctx.GetSessionVars().GetCharsetInfo()
				vars.Value = ast.NewValueExpr(cn.Name.Name.O, char, col)
			}
			mockTablePlan := LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
			var err error
			assign.Expr, _, err = b.rewrite(ctx, vars.Value, mockTablePlan, nil, true)
			if err != nil {
				return nil, err
			}
		} else {
			assign.IsDefault = true
		}
		if vars.ExtendValue != nil {
			assign.ExtendValue = &expression.Constant{
				Value:   vars.ExtendValue.(*driver.ValueExpr).Datum,
				RetType: &vars.ExtendValue.(*driver.ValueExpr).Type,
			}
		}
		p.VarAssigns = append(p.VarAssigns, assign)
	}
	return p, nil
}

func (b *PlanBuilder) buildDropBindPlan(v *ast.DropBindingStmt) (Plan, error) {
	p := &SQLBindPlan{
		SQLBindOp:    OpSQLBindDrop,
		NormdOrigSQL: parser.Normalize(v.OriginSel.Text()),
		IsGlobal:     v.GlobalScope,
		Db:           utilparser.GetDefaultDB(v.OriginSel, b.ctx.GetSessionVars().CurrentDB),
	}
	if v.HintedSel != nil {
		p.BindSQL = v.HintedSel.Text()
	}
	b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", nil)
	return p, nil
}

func (b *PlanBuilder) buildCreateBindPlan(v *ast.CreateBindingStmt) (Plan, error) {
	charSet, collation := b.ctx.GetSessionVars().GetCharsetInfo()
	p := &SQLBindPlan{
		SQLBindOp:    OpSQLBindCreate,
		NormdOrigSQL: parser.Normalize(v.OriginNode.Text()),
		BindSQL:      v.HintedNode.Text(),
		IsGlobal:     v.GlobalScope,
		BindStmt:     v.HintedNode,
		Db:           utilparser.GetDefaultDB(v.OriginNode, b.ctx.GetSessionVars().CurrentDB),
		Charset:      charSet,
		Collation:    collation,
	}
	b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", nil)
	return p, nil
}

// detectSelectAgg detects an aggregate function or GROUP BY clause.
func (b *PlanBuilder) detectSelectAgg(sel *ast.SelectStmt) bool {
	if sel.GroupBy != nil {
		return true
	}
	for _, f := range sel.Fields.Fields {
		if ast.HasAggFlag(f.Expr) {
			return true
		}
	}
	if sel.Having != nil {
		if ast.HasAggFlag(sel.Having.Expr) {
			return true
		}
	}
	if sel.OrderBy != nil {
		for _, item := range sel.OrderBy.Items {
			if ast.HasAggFlag(item.Expr) {
				return true
			}
		}
	}
	return false
}

func (b *PlanBuilder) detectSelectWindow(sel *ast.SelectStmt) bool {
	for _, f := range sel.Fields.Fields {
		if ast.HasWindowFlag(f.Expr) {
			return true
		}
	}
	if sel.OrderBy != nil {
		for _, item := range sel.OrderBy.Items {
			if ast.HasWindowFlag(item.Expr) {
				return true
			}
		}
	}
	return false
}

func getPathByIndexName(paths []*util.AccessPath, idxName model.CIStr, tblInfo *model.TableInfo) *util.AccessPath {
	var tablePath *util.AccessPath
	for _, path := range paths {
		if path.IsTablePath() {
			tablePath = path
			continue
		}
		if path.Index.Name.L == idxName.L {
			return path
		}
	}
	if isPrimaryIndex(idxName) && (tblInfo.PKIsHandle || tblInfo.IsCommonHandle) {
		return tablePath
	}
	return nil
}

func isPrimaryIndex(indexName model.CIStr) bool {
	return indexName.L == "primary"
}

func genTiFlashPath(tblInfo *model.TableInfo, isGlobalRead bool) *util.AccessPath {
	tiFlashPath := &util.AccessPath{StoreType: kv.TiFlash, IsTiFlashGlobalRead: isGlobalRead}
	fillContentForTablePath(tiFlashPath, tblInfo)
	return tiFlashPath
}

func fillContentForTablePath(tablePath *util.AccessPath, tblInfo *model.TableInfo) {
	if tblInfo.IsCommonHandle {
		tablePath.IsCommonHandlePath = true
		for _, index := range tblInfo.Indices {
			if index.Primary {
				tablePath.Index = index
				break
			}
		}
	} else {
		tablePath.IsIntHandlePath = true
	}
}

func getPossibleAccessPaths(ctx sessionctx.Context, tableHints *tableHintInfo, indexHints []*ast.IndexHint, tbl table.Table, dbName, tblName model.CIStr) ([]*util.AccessPath, error) {
	tblInfo := tbl.Meta()
	publicPaths := make([]*util.AccessPath, 0, len(tblInfo.Indices)+2)
	tp := kv.TiKV
	if tbl.Type().IsClusterTable() {
		tp = kv.TiDB
	}
	tablePath := &util.AccessPath{StoreType: tp}
	fillContentForTablePath(tablePath, tblInfo)
	publicPaths = append(publicPaths, tablePath)
	if tblInfo.TiFlashReplica != nil && tblInfo.TiFlashReplica.Available {
		publicPaths = append(publicPaths, genTiFlashPath(tblInfo, false))
		publicPaths = append(publicPaths, genTiFlashPath(tblInfo, true))
	}
	optimizerUseInvisibleIndexes := ctx.GetSessionVars().OptimizerUseInvisibleIndexes
	for _, index := range tblInfo.Indices {
		if index.State == model.StatePublic {
			// Filter out invisible index, because they are not visible for optimizer
			if !optimizerUseInvisibleIndexes && index.Invisible {
				continue
			}
			if tblInfo.IsCommonHandle && index.Primary {
				continue
			}
			publicPaths = append(publicPaths, &util.AccessPath{Index: index})
		}
	}

	hasScanHint, hasUseOrForce := false, false
	available := make([]*util.AccessPath, 0, len(publicPaths))
	ignored := make([]*util.AccessPath, 0, len(publicPaths))

	// Extract comment-style index hint like /*+ INDEX(t, idx1, idx2) */.
	indexHintsLen := len(indexHints)
	if tableHints != nil {
		for i, hint := range tableHints.indexHintList {
			if hint.dbName.L == dbName.L && hint.tblName.L == tblName.L {
				indexHints = append(indexHints, hint.indexHint)
				tableHints.indexHintList[i].matched = true
			}
		}
	}

	_, isolationReadEnginesHasTiKV := ctx.GetSessionVars().GetIsolationReadEngines()[kv.TiKV]
	for i, hint := range indexHints {
		if hint.HintScope != ast.HintForScan {
			continue
		}

		hasScanHint = true

		if !isolationReadEnginesHasTiKV {
			if hint.IndexNames != nil {
				engineVals, _ := ctx.GetSessionVars().GetSystemVar(variable.TiDBIsolationReadEngines)
				err := errors.New(fmt.Sprintf("TiDB doesn't support index in the isolation read engines(value: '%v')", engineVals))
				if i < indexHintsLen {
					return nil, err
				}
				ctx.GetSessionVars().StmtCtx.AppendWarning(err)
			}
			continue
		}
		// It is syntactically valid to omit index_list for USE INDEX, which means “use no indexes”.
		// Omitting index_list for FORCE INDEX or IGNORE INDEX is a syntax error.
		// See https://dev.mysql.com/doc/refman/8.0/en/index-hints.html.
		if hint.IndexNames == nil && hint.HintType != ast.HintIgnore {
			if path := getTablePath(publicPaths); path != nil {
				hasUseOrForce = true
				path.Forced = true
				available = append(available, path)
			}
		}
		for _, idxName := range hint.IndexNames {
			path := getPathByIndexName(publicPaths, idxName, tblInfo)
			if path == nil {
				err := ErrKeyDoesNotExist.GenWithStackByArgs(idxName, tblInfo.Name)
				// if hint is from comment-style sql hints, we should throw a warning instead of error.
				if i < indexHintsLen {
					return nil, err
				}
				ctx.GetSessionVars().StmtCtx.AppendWarning(err)
				continue
			}
			if hint.HintType == ast.HintIgnore {
				// Collect all the ignored index hints.
				ignored = append(ignored, path)
				continue
			}
			// Currently we don't distinguish between "FORCE" and "USE" because
			// our cost estimation is not reliable.
			hasUseOrForce = true
			path.Forced = true
			available = append(available, path)
		}
	}

	if !hasScanHint || !hasUseOrForce {
		available = publicPaths
	}

	available = removeIgnoredPaths(available, ignored, tblInfo)

	// If we have got "FORCE" or "USE" index hint but got no available index,
	// we have to use table scan.
	if len(available) == 0 {
		available = append(available, tablePath)
	}
	return available, nil
}

func filterPathByIsolationRead(ctx sessionctx.Context, paths []*util.AccessPath, dbName model.CIStr) ([]*util.AccessPath, error) {
	// TODO: filter paths with isolation read locations.
	if dbName.L == mysql.SystemDB {
		return paths, nil
	}
	isolationReadEngines := ctx.GetSessionVars().GetIsolationReadEngines()
	availableEngine := map[kv.StoreType]struct{}{}
	var availableEngineStr string
	for i := len(paths) - 1; i >= 0; i-- {
		if _, ok := availableEngine[paths[i].StoreType]; !ok {
			availableEngine[paths[i].StoreType] = struct{}{}
			if availableEngineStr != "" {
				availableEngineStr += ", "
			}
			availableEngineStr += paths[i].StoreType.Name()
		}
		if _, ok := isolationReadEngines[paths[i].StoreType]; !ok && paths[i].StoreType != kv.TiDB {
			paths = append(paths[:i], paths[i+1:]...)
		}
	}
	var err error
	if len(paths) == 0 {
		engineVals, _ := ctx.GetSessionVars().GetSystemVar(variable.TiDBIsolationReadEngines)
		err = ErrInternal.GenWithStackByArgs(fmt.Sprintf("Can not find access path matching '%v'(value: '%v'). Available values are '%v'.",
			variable.TiDBIsolationReadEngines, engineVals, availableEngineStr))
	}
	return paths, err
}

func removeIgnoredPaths(paths, ignoredPaths []*util.AccessPath, tblInfo *model.TableInfo) []*util.AccessPath {
	if len(ignoredPaths) == 0 {
		return paths
	}
	remainedPaths := make([]*util.AccessPath, 0, len(paths))
	for _, path := range paths {
		if path.IsTablePath() || getPathByIndexName(ignoredPaths, path.Index.Name, tblInfo) == nil {
			remainedPaths = append(remainedPaths, path)
		}
	}
	return remainedPaths
}

func (b *PlanBuilder) buildSelectLock(src LogicalPlan, lock *ast.SelectLockInfo) *LogicalLock {
	selectLock := LogicalLock{
		Lock:             lock,
		tblID2Handle:     b.handleHelper.tailMap(),
		partitionedTable: b.partitionedTable,
	}.Init(b.ctx)
	selectLock.SetChildren(src)
	return selectLock
}

func (b *PlanBuilder) buildPrepare(x *ast.PrepareStmt) Plan {
	p := &Prepare{
		Name: x.Name,
	}
	if x.SQLVar != nil {
		if v, ok := b.ctx.GetSessionVars().Users[strings.ToLower(x.SQLVar.Name)]; ok {
			p.SQLText = v.GetString()
		} else {
			p.SQLText = "NULL"
		}
	} else {
		p.SQLText = x.SQLText
	}
	return p
}

func (b *PlanBuilder) buildAdmin(ctx context.Context, as *ast.AdminStmt) (Plan, error) {
	var ret Plan
	var err error
	switch as.Tp {
	case ast.AdminCheckTable, ast.AdminCheckIndex:
		ret, err = b.buildAdminCheckTable(ctx, as)
		if err != nil {
			return ret, err
		}
	case ast.AdminRecoverIndex:
		p := &RecoverIndex{Table: as.Tables[0], IndexName: as.Index}
		p.setSchemaAndNames(buildRecoverIndexFields())
		ret = p
	case ast.AdminCleanupIndex:
		p := &CleanupIndex{Table: as.Tables[0], IndexName: as.Index}
		p.setSchemaAndNames(buildCleanupIndexFields())
		ret = p
	case ast.AdminChecksumTable:
		p := &ChecksumTable{Tables: as.Tables}
		p.setSchemaAndNames(buildChecksumTableSchema())
		ret = p
	case ast.AdminShowNextRowID:
		p := &ShowNextRowID{TableName: as.Tables[0]}
		p.setSchemaAndNames(buildShowNextRowID())
		ret = p
	case ast.AdminShowDDL:
		p := &ShowDDL{}
		p.setSchemaAndNames(buildShowDDLFields())
		ret = p
	case ast.AdminShowDDLJobs:
		p := LogicalShowDDLJobs{JobNumber: as.JobNumber}.Init(b.ctx)
		p.setSchemaAndNames(buildShowDDLJobsFields())
		for _, col := range p.schema.Columns {
			col.UniqueID = b.ctx.GetSessionVars().AllocPlanColumnID()
		}
		ret = p
		if as.Where != nil {
			ret, err = b.buildSelection(ctx, p, as.Where, nil)
			if err != nil {
				return nil, err
			}
		}
	case ast.AdminCancelDDLJobs:
		p := &CancelDDLJobs{JobIDs: as.JobIDs}
		p.setSchemaAndNames(buildCancelDDLJobsFields())
		ret = p
	case ast.AdminCheckIndexRange:
		schema, names, err := b.buildCheckIndexSchema(as.Tables[0], as.Index)
		if err != nil {
			return nil, err
		}

		p := &CheckIndexRange{Table: as.Tables[0], IndexName: as.Index, HandleRanges: as.HandleRanges}
		p.setSchemaAndNames(schema, names)
		ret = p
	case ast.AdminShowDDLJobQueries:
		p := &ShowDDLJobQueries{JobIDs: as.JobIDs}
		p.setSchemaAndNames(buildShowDDLJobQueriesFields())
		ret = p
	case ast.AdminShowSlow:
		p := &ShowSlow{ShowSlow: as.ShowSlow}
		p.setSchemaAndNames(buildShowSlowSchema())
		ret = p
	case ast.AdminReloadExprPushdownBlacklist:
		return &ReloadExprPushdownBlacklist{}, nil
	case ast.AdminReloadOptRuleBlacklist:
		return &ReloadOptRuleBlacklist{}, nil
	case ast.AdminPluginEnable:
		return &AdminPlugins{Action: Enable, Plugins: as.Plugins}, nil
	case ast.AdminPluginDisable:
		return &AdminPlugins{Action: Disable, Plugins: as.Plugins}, nil
	case ast.AdminFlushBindings:
		return &SQLBindPlan{SQLBindOp: OpFlushBindings}, nil
	case ast.AdminCaptureBindings:
		return &SQLBindPlan{SQLBindOp: OpCaptureBindings}, nil
	case ast.AdminEvolveBindings:
		return &SQLBindPlan{SQLBindOp: OpEvolveBindings}, nil
	case ast.AdminReloadBindings:
		return &SQLBindPlan{SQLBindOp: OpReloadBindings}, nil
	case ast.AdminShowTelemetry:
		p := &AdminShowTelemetry{}
		p.setSchemaAndNames(buildShowTelemetrySchema())
		ret = p
	case ast.AdminResetTelemetryID:
		return &AdminResetTelemetryID{}, nil
	case ast.AdminReloadStatistics:
		return &Simple{Statement: as}, nil
	default:
		return nil, ErrUnsupportedType.GenWithStack("Unsupported ast.AdminStmt(%T) for buildAdmin", as)
	}

	// Admin command can only be executed by administrator.
	b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", nil)
	return ret, nil
}

// getGenExprs gets generated expressions map.
func (b *PlanBuilder) getGenExprs(ctx context.Context, dbName model.CIStr, tbl table.Table, idx *model.IndexInfo, exprCols *expression.Schema, names types.NameSlice) (
	map[model.TableColumnID]expression.Expression, error) {
	tblInfo := tbl.Meta()
	genExprsMap := make(map[model.TableColumnID]expression.Expression)
	exprs := make([]expression.Expression, 0, len(tbl.Cols()))
	genExprIdxs := make([]model.TableColumnID, len(tbl.Cols()))
	mockTablePlan := LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
	mockTablePlan.SetSchema(exprCols)
	mockTablePlan.names = names
	for i, colExpr := range mockTablePlan.Schema().Columns {
		col := tbl.Cols()[i]
		var expr expression.Expression
		expr = colExpr
		if col.IsGenerated() && !col.GeneratedStored {
			var err error
			expr, _, err = b.rewrite(ctx, col.GeneratedExpr, mockTablePlan, nil, true)
			if err != nil {
				return nil, errors.Trace(err)
			}
			found := false
			for _, column := range idx.Columns {
				if strings.EqualFold(col.Name.L, column.Name.L) {
					found = true
					break
				}
			}
			if found {
				genColumnID := model.TableColumnID{TableID: tblInfo.ID, ColumnID: col.ColumnInfo.ID}
				genExprsMap[genColumnID] = expr
				genExprIdxs[i] = genColumnID
			}
		}
		exprs = append(exprs, expr)
	}
	// Re-iterate expressions to handle those virtual generated columns that refers to the other generated columns.
	for i, expr := range exprs {
		exprs[i] = expression.ColumnSubstitute(expr, mockTablePlan.Schema(), exprs)
		if _, ok := genExprsMap[genExprIdxs[i]]; ok {
			genExprsMap[genExprIdxs[i]] = exprs[i]
		}
	}
	return genExprsMap, nil
}

// FindColumnInfoByID finds ColumnInfo in cols by ID.
func FindColumnInfoByID(colInfos []*model.ColumnInfo, id int64) *model.ColumnInfo {
	for _, info := range colInfos {
		if info.ID == id {
			return info
		}
	}
	return nil
}

func (b *PlanBuilder) buildPhysicalIndexLookUpReader(ctx context.Context, dbName model.CIStr, tbl table.Table, idx *model.IndexInfo) (Plan, error) {
	tblInfo := tbl.Meta()
	physicalID, isPartition := getPhysicalID(tbl)
	fullExprCols, _, err := expression.TableInfo2SchemaAndNames(b.ctx, dbName, tblInfo)
	if err != nil {
		return nil, err
	}
	extraInfo, extraCol, hasExtraCol := tryGetPkExtraColumn(b.ctx.GetSessionVars(), tblInfo)
	pkHandleInfo, pkHandleCol, hasPkIsHandle := tryGetPkHandleCol(tblInfo, fullExprCols)
	commonInfos, commonCols, hasCommonCols := tryGetCommonHandleCols(tbl, fullExprCols)
	idxColInfos := getIndexColumnInfos(tblInfo, idx)
	idxColSchema := getIndexColsSchema(tblInfo, idx, fullExprCols)
	idxCols, idxColLens := expression.IndexInfo2PrefixCols(idxColInfos, idxColSchema.Columns, idx)

	is := PhysicalIndexScan{
		Table:            tblInfo,
		TableAsName:      &tblInfo.Name,
		DBName:           dbName,
		Columns:          idxColInfos,
		Index:            idx,
		IdxCols:          idxCols,
		IdxColLens:       idxColLens,
		dataSourceSchema: idxColSchema.Clone(),
		Ranges:           ranger.FullRange(),
		physicalTableID:  physicalID,
		isPartition:      isPartition,
	}.Init(b.ctx, b.getSelectOffset())
	// There is no alternative plan choices, so just use pseudo stats to avoid panic.
	is.stats = &property.StatsInfo{HistColl: &(statistics.PseudoTable(tblInfo)).HistColl}
	if hasCommonCols {
		for _, c := range commonInfos {
			is.Columns = append(is.Columns, c.ColumnInfo)
		}
	}
	is.initSchema(append(is.IdxCols, commonCols...), true)

	// It's double read case.
	ts := PhysicalTableScan{
		Columns:         idxColInfos,
		Table:           tblInfo,
		TableAsName:     &tblInfo.Name,
		physicalTableID: physicalID,
		isPartition:     isPartition,
	}.Init(b.ctx, b.getSelectOffset())
	ts.SetSchema(idxColSchema)
	ts.Columns = ExpandVirtualColumn(ts.Columns, ts.schema, ts.Table.Columns)
	switch {
	case hasExtraCol:
		ts.Columns = append(ts.Columns, extraInfo)
		ts.schema.Append(extraCol)
		ts.HandleIdx = []int{len(ts.Columns) - 1}
	case hasPkIsHandle:
		ts.Columns = append(ts.Columns, pkHandleInfo)
		ts.schema.Append(pkHandleCol)
		ts.HandleIdx = []int{len(ts.Columns) - 1}
	case hasCommonCols:
		ts.HandleIdx = make([]int, 0, len(commonCols))
		for pkOffset, cInfo := range commonInfos {
			found := false
			for i, c := range ts.Columns {
				if c.ID == cInfo.ID {
					found = true
					ts.HandleIdx = append(ts.HandleIdx, i)
					break
				}
			}
			if !found {
				ts.Columns = append(ts.Columns, cInfo.ColumnInfo)
				ts.schema.Append(commonCols[pkOffset])
				ts.HandleIdx = append(ts.HandleIdx, len(ts.Columns)-1)
			}

		}
	}

	cop := &copTask{
		indexPlan:        is,
		tablePlan:        ts,
		tblColHists:      is.stats.HistColl,
		extraHandleCol:   extraCol,
		commonHandleCols: commonCols,
	}
	rootT := finishCopTask(b.ctx, cop).(*rootTask)
	if err := rootT.p.ResolveIndices(); err != nil {
		return nil, err
	}
	return rootT.p, nil
}

func getIndexColumnInfos(tblInfo *model.TableInfo, idx *model.IndexInfo) []*model.ColumnInfo {
	ret := make([]*model.ColumnInfo, len(idx.Columns))
	for i, idxCol := range idx.Columns {
		ret[i] = tblInfo.Columns[idxCol.Offset]
	}
	return ret
}

func getIndexColsSchema(tblInfo *model.TableInfo, idx *model.IndexInfo, allColSchema *expression.Schema) *expression.Schema {
	schema := expression.NewSchema(make([]*expression.Column, 0, len(idx.Columns))...)
	for _, idxCol := range idx.Columns {
		for i, colInfo := range tblInfo.Columns {
			if colInfo.Name.L == idxCol.Name.L {
				schema.Append(allColSchema.Columns[i])
				break
			}
		}
	}
	return schema
}

func getPhysicalID(t table.Table) (physicalID int64, isPartition bool) {
	tblInfo := t.Meta()
	if tblInfo.GetPartitionInfo() != nil {
		pid := t.(table.PhysicalTable).GetPhysicalID()
		return pid, true
	}
	return tblInfo.ID, false
}

func tryGetPkExtraColumn(sv *variable.SessionVars, tblInfo *model.TableInfo) (*model.ColumnInfo, *expression.Column, bool) {
	if tblInfo.IsCommonHandle || tblInfo.PKIsHandle {
		return nil, nil, false
	}
	info := model.NewExtraHandleColInfo()
	expCol := &expression.Column{
		RetType:  types.NewFieldType(mysql.TypeLonglong),
		UniqueID: sv.AllocPlanColumnID(),
		ID:       model.ExtraHandleID,
	}
	return info, expCol, true
}

func tryGetCommonHandleCols(t table.Table, allColSchema *expression.Schema) ([]*table.Column, []*expression.Column, bool) {
	tblInfo := t.Meta()
	if !tblInfo.IsCommonHandle {
		return nil, nil, false
	}
	pk := tables.FindPrimaryIndex(tblInfo)
	commonHandleCols, _ := expression.IndexInfo2Cols(tblInfo.Columns, allColSchema.Columns, pk)
	commonHandelColInfos := tables.TryGetCommonPkColumns(t)
	return commonHandelColInfos, commonHandleCols, true
}

func tryGetPkHandleCol(tblInfo *model.TableInfo, allColSchema *expression.Schema) (*model.ColumnInfo, *expression.Column, bool) {
	if !tblInfo.PKIsHandle {
		return nil, nil, false
	}
	for i, c := range tblInfo.Columns {
		if mysql.HasPriKeyFlag(c.Flag) {
			return c, allColSchema.Columns[i], true
		}
	}
	return nil, nil, false
}

func (b *PlanBuilder) buildPhysicalIndexLookUpReaders(ctx context.Context, dbName model.CIStr, tbl table.Table, indices []table.Index) ([]Plan, []*model.IndexInfo, error) {
	tblInfo := tbl.Meta()
	// get index information
	indexInfos := make([]*model.IndexInfo, 0, len(tblInfo.Indices))
	indexLookUpReaders := make([]Plan, 0, len(tblInfo.Indices))
	for _, idx := range indices {
		idxInfo := idx.Meta()
		if idxInfo.State != model.StatePublic {
			logutil.Logger(context.Background()).Info("build physical index lookup reader, the index isn't public",
				zap.String("index", idxInfo.Name.O), zap.Stringer("state", idxInfo.State), zap.String("table", tblInfo.Name.O))
			continue
		}
		indexInfos = append(indexInfos, idxInfo)
		// For partition tables.
		if pi := tbl.Meta().GetPartitionInfo(); pi != nil {
			for _, def := range pi.Definitions {
				t := tbl.(table.PartitionedTable).GetPartition(def.ID)
				reader, err := b.buildPhysicalIndexLookUpReader(ctx, dbName, t, idxInfo)
				if err != nil {
					return nil, nil, err
				}
				indexLookUpReaders = append(indexLookUpReaders, reader)
			}
			continue
		}
		// For non-partition tables.
		reader, err := b.buildPhysicalIndexLookUpReader(ctx, dbName, tbl, idxInfo)
		if err != nil {
			return nil, nil, err
		}
		indexLookUpReaders = append(indexLookUpReaders, reader)
	}
	if len(indexLookUpReaders) == 0 {
		return nil, nil, nil
	}
	return indexLookUpReaders, indexInfos, nil
}

func (b *PlanBuilder) buildAdminCheckTable(ctx context.Context, as *ast.AdminStmt) (*CheckTable, error) {
	tblName := as.Tables[0]
	tableInfo := as.Tables[0].TableInfo
	tbl, ok := b.is.TableByID(tableInfo.ID)
	if !ok {
		return nil, infoschema.ErrTableNotExists.GenWithStackByArgs(tblName.DBInfo.Name.O, tableInfo.Name.O)
	}
	p := &CheckTable{
		DBName: tblName.Schema.O,
		Table:  tbl,
	}
	var readerPlans []Plan
	var indexInfos []*model.IndexInfo
	var err error
	if as.Tp == ast.AdminCheckIndex {
		// get index information
		var idx table.Index
		idxName := strings.ToLower(as.Index)
		for _, index := range tbl.Indices() {
			if index.Meta().Name.L == idxName {
				idx = index
				break
			}
		}
		if idx == nil {
			return nil, errors.Errorf("index %s do not exist", as.Index)
		}
		if idx.Meta().State != model.StatePublic {
			return nil, errors.Errorf("index %s state %s isn't public", as.Index, idx.Meta().State)
		}
		p.CheckIndex = true
		readerPlans, indexInfos, err = b.buildPhysicalIndexLookUpReaders(ctx, tblName.Schema, tbl, []table.Index{idx})
	} else {
		readerPlans, indexInfos, err = b.buildPhysicalIndexLookUpReaders(ctx, tblName.Schema, tbl, tbl.Indices())
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	readers := make([]*PhysicalIndexLookUpReader, 0, len(readerPlans))
	for _, plan := range readerPlans {
		readers = append(readers, plan.(*PhysicalIndexLookUpReader))
	}
	p.IndexInfos = indexInfos
	p.IndexLookUpReaders = readers
	return p, nil
}

func (b *PlanBuilder) buildCheckIndexSchema(tn *ast.TableName, indexName string) (*expression.Schema, types.NameSlice, error) {
	schema := expression.NewSchema()
	var names types.NameSlice
	indexName = strings.ToLower(indexName)
	indicesInfo := tn.TableInfo.Indices
	cols := tn.TableInfo.Cols()
	for _, idxInfo := range indicesInfo {
		if idxInfo.Name.L != indexName {
			continue
		}
		for _, idxCol := range idxInfo.Columns {
			col := cols[idxCol.Offset]
			names = append(names, &types.FieldName{
				ColName: idxCol.Name,
				TblName: tn.Name,
				DBName:  tn.Schema,
			})
			schema.Append(&expression.Column{
				RetType:  &col.FieldType,
				UniqueID: b.ctx.GetSessionVars().AllocPlanColumnID(),
				ID:       col.ID})
		}
		names = append(names, &types.FieldName{
			ColName: model.NewCIStr("extra_handle"),
			TblName: tn.Name,
			DBName:  tn.Schema,
		})
		schema.Append(&expression.Column{
			RetType:  types.NewFieldType(mysql.TypeLonglong),
			UniqueID: b.ctx.GetSessionVars().AllocPlanColumnID(),
			ID:       -1,
		})
	}
	if schema.Len() == 0 {
		return nil, nil, errors.Errorf("index %s not found", indexName)
	}
	return schema, names, nil
}

// getColsInfo returns the info of index columns, normal columns and primary key.
func getColsInfo(tn *ast.TableName) (indicesInfo []*model.IndexInfo, colsInfo []*model.ColumnInfo) {
	tbl := tn.TableInfo
	for _, col := range tbl.Columns {
		// The virtual column will not store any data in TiKV, so it should be ignored when collect statistics
		if col.IsGenerated() && !col.GeneratedStored {
			continue
		}
		if mysql.HasPriKeyFlag(col.Flag) && (tbl.PKIsHandle || tbl.IsCommonHandle) {
			continue
		}
		colsInfo = append(colsInfo, col)
	}
	for _, idx := range tn.TableInfo.Indices {
		if idx.State == model.StatePublic {
			indicesInfo = append(indicesInfo, idx)
		}
	}
	return
}

// BuildHandleColsForAnalyze is exported for test.
func BuildHandleColsForAnalyze(ctx sessionctx.Context, tblInfo *model.TableInfo) HandleCols {
	var handleCols HandleCols
	switch {
	case tblInfo.PKIsHandle:
		pkCol := tblInfo.GetPkColInfo()
		handleCols = &IntHandleCols{col: &expression.Column{
			ID:      pkCol.ID,
			RetType: &pkCol.FieldType,
			Index:   pkCol.Offset,
		}}
	case tblInfo.IsCommonHandle:
		pkIdx := tables.FindPrimaryIndex(tblInfo)
		pkColLen := len(pkIdx.Columns)
		columns := make([]*expression.Column, pkColLen)
		for i := 0; i < pkColLen; i++ {
			colInfo := tblInfo.Columns[pkIdx.Columns[i].Offset]
			columns[i] = &expression.Column{
				ID:      colInfo.ID,
				RetType: &colInfo.FieldType,
				Index:   colInfo.Offset,
			}
		}
		handleCols = &CommonHandleCols{
			tblInfo: tblInfo,
			idxInfo: pkIdx,
			columns: columns,
			sc:      ctx.GetSessionVars().StmtCtx,
		}
	}
	return handleCols
}

func getPhysicalIDsAndPartitionNames(tblInfo *model.TableInfo, partitionNames []model.CIStr) ([]int64, []string, error) {
	pi := tblInfo.GetPartitionInfo()
	if pi == nil {
		if len(partitionNames) != 0 {
			return nil, nil, errors.Trace(ddl.ErrPartitionMgmtOnNonpartitioned)
		}
		return []int64{tblInfo.ID}, []string{""}, nil
	}
	if len(partitionNames) == 0 {
		ids := make([]int64, 0, len(pi.Definitions))
		names := make([]string, 0, len(pi.Definitions))
		for _, def := range pi.Definitions {
			ids = append(ids, def.ID)
			names = append(names, def.Name.O)
		}
		return ids, names, nil
	}
	ids := make([]int64, 0, len(partitionNames))
	names := make([]string, 0, len(partitionNames))
	for _, name := range partitionNames {
		found := false
		for _, def := range pi.Definitions {
			if def.Name.L == name.L {
				found = true
				ids = append(ids, def.ID)
				names = append(names, def.Name.O)
				break
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("can not found the specified partition name %s in the table definition", name.O)
		}
	}
	return ids, names, nil
}

func (b *PlanBuilder) buildAnalyzeTable(as *ast.AnalyzeTableStmt, opts map[ast.AnalyzeOptionType]uint64) (Plan, error) {
	p := &Analyze{Opts: opts}
	for _, tbl := range as.TableNames {
		if tbl.TableInfo.IsView() {
			return nil, errors.Errorf("analyze view %s is not supported now.", tbl.Name.O)
		}
		if tbl.TableInfo.IsSequence() {
			return nil, errors.Errorf("analyze sequence %s is not supported now.", tbl.Name.O)
		}
		idxInfo, colInfo := getColsInfo(tbl)
		physicalIDs, names, err := getPhysicalIDsAndPartitionNames(tbl.TableInfo, as.PartitionNames)
		if err != nil {
			return nil, err
		}
		for _, idx := range idxInfo {
			for i, id := range physicalIDs {
				info := analyzeInfo{DBName: tbl.Schema.O, TableName: tbl.Name.O, PartitionName: names[i], TableID: AnalyzeTableID{PersistID: id, CollectIDs: []int64{id}}, Incremental: as.Incremental}
				p.IdxTasks = append(p.IdxTasks, AnalyzeIndexTask{
					IndexInfo:   idx,
					analyzeInfo: info,
					TblInfo:     tbl.TableInfo,
				})
			}
		}
		handleCols := BuildHandleColsForAnalyze(b.ctx, tbl.TableInfo)
		if len(colInfo) > 0 || handleCols != nil {
			for i, id := range physicalIDs {
				info := analyzeInfo{DBName: tbl.Schema.O, TableName: tbl.Name.O, PartitionName: names[i], TableID: AnalyzeTableID{PersistID: id, CollectIDs: []int64{id}}, Incremental: as.Incremental}
				p.ColTasks = append(p.ColTasks, AnalyzeColumnsTask{
					HandleCols:  handleCols,
					ColsInfo:    colInfo,
					analyzeInfo: info,
					TblInfo:     tbl.TableInfo,
				})
			}
		}
	}
	return p, nil
}

func (b *PlanBuilder) buildAnalyzeIndex(as *ast.AnalyzeTableStmt, opts map[ast.AnalyzeOptionType]uint64) (Plan, error) {
	p := &Analyze{Opts: opts}
	tblInfo := as.TableNames[0].TableInfo
	physicalIDs, names, err := getPhysicalIDsAndPartitionNames(tblInfo, as.PartitionNames)
	if err != nil {
		return nil, err
	}
	for _, idxName := range as.IndexNames {
		if isPrimaryIndex(idxName) {
			handleCols := BuildHandleColsForAnalyze(b.ctx, tblInfo)
			if handleCols != nil {
				for i, id := range physicalIDs {
					info := analyzeInfo{DBName: as.TableNames[0].Schema.O, TableName: as.TableNames[0].Name.O, PartitionName: names[i], TableID: AnalyzeTableID{PersistID: id, CollectIDs: []int64{id}}, Incremental: as.Incremental}
					p.ColTasks = append(p.ColTasks, AnalyzeColumnsTask{HandleCols: handleCols, analyzeInfo: info, TblInfo: tblInfo})
				}
				continue
			}
		}
		idx := tblInfo.FindIndexByName(idxName.L)
		if idx == nil || idx.State != model.StatePublic {
			return nil, ErrAnalyzeMissIndex.GenWithStackByArgs(idxName.O, tblInfo.Name.O)
		}
		for i, id := range physicalIDs {
			info := analyzeInfo{DBName: as.TableNames[0].Schema.O, TableName: as.TableNames[0].Name.O, PartitionName: names[i], TableID: AnalyzeTableID{PersistID: id, CollectIDs: []int64{id}}, Incremental: as.Incremental}
			p.IdxTasks = append(p.IdxTasks, AnalyzeIndexTask{IndexInfo: idx, analyzeInfo: info, TblInfo: tblInfo})
		}
	}
	return p, nil
}

func (b *PlanBuilder) buildAnalyzeAllIndex(as *ast.AnalyzeTableStmt, opts map[ast.AnalyzeOptionType]uint64) (Plan, error) {
	p := &Analyze{Opts: opts}
	tblInfo := as.TableNames[0].TableInfo
	physicalIDs, names, err := getPhysicalIDsAndPartitionNames(tblInfo, as.PartitionNames)
	if err != nil {
		return nil, err
	}
	for _, idx := range tblInfo.Indices {
		if idx.State == model.StatePublic {
			for i, id := range physicalIDs {
				info := analyzeInfo{DBName: as.TableNames[0].Schema.O, TableName: as.TableNames[0].Name.O, PartitionName: names[i], TableID: AnalyzeTableID{PersistID: id, CollectIDs: []int64{id}}, Incremental: as.Incremental}
				p.IdxTasks = append(p.IdxTasks, AnalyzeIndexTask{IndexInfo: idx, analyzeInfo: info, TblInfo: tblInfo})
			}
		}
	}
	handleCols := BuildHandleColsForAnalyze(b.ctx, tblInfo)
	if handleCols != nil {
		for i, id := range physicalIDs {
			info := analyzeInfo{DBName: as.TableNames[0].Schema.O, TableName: as.TableNames[0].Name.O, PartitionName: names[i], TableID: AnalyzeTableID{PersistID: id, CollectIDs: []int64{id}}, Incremental: as.Incremental}
			p.ColTasks = append(p.ColTasks, AnalyzeColumnsTask{HandleCols: handleCols, analyzeInfo: info, TblInfo: tblInfo})
		}
	}
	return p, nil
}

var cmSketchSizeLimit = kv.TxnEntrySizeLimit / binary.MaxVarintLen32

var analyzeOptionLimit = map[ast.AnalyzeOptionType]uint64{
	ast.AnalyzeOptNumBuckets:    1024,
	ast.AnalyzeOptNumTopN:       1024,
	ast.AnalyzeOptCMSketchWidth: cmSketchSizeLimit,
	ast.AnalyzeOptCMSketchDepth: cmSketchSizeLimit,
	ast.AnalyzeOptNumSamples:    100000,
}

var analyzeOptionDefault = map[ast.AnalyzeOptionType]uint64{
	ast.AnalyzeOptNumBuckets:    256,
	ast.AnalyzeOptNumTopN:       20,
	ast.AnalyzeOptCMSketchWidth: 2048,
	ast.AnalyzeOptCMSketchDepth: 5,
	ast.AnalyzeOptNumSamples:    10000,
}

func handleAnalyzeOptions(opts []ast.AnalyzeOpt) (map[ast.AnalyzeOptionType]uint64, error) {
	optMap := make(map[ast.AnalyzeOptionType]uint64, len(analyzeOptionDefault))
	for key, val := range analyzeOptionDefault {
		optMap[key] = val
	}
	for _, opt := range opts {
		if opt.Type == ast.AnalyzeOptNumTopN {
			if opt.Value > analyzeOptionLimit[opt.Type] {
				return nil, errors.Errorf("value of analyze option %s should not larger than %d", ast.AnalyzeOptionString[opt.Type], analyzeOptionLimit[opt.Type])
			}
		} else {
			if opt.Value == 0 || opt.Value > analyzeOptionLimit[opt.Type] {
				return nil, errors.Errorf("value of analyze option %s should be positive and not larger than %d", ast.AnalyzeOptionString[opt.Type], analyzeOptionLimit[opt.Type])
			}
		}
		optMap[opt.Type] = opt.Value
	}
	if optMap[ast.AnalyzeOptCMSketchWidth]*optMap[ast.AnalyzeOptCMSketchDepth] > cmSketchSizeLimit {
		return nil, errors.Errorf("cm sketch size(depth * width) should not larger than %d", cmSketchSizeLimit)
	}
	return optMap, nil
}

func (b *PlanBuilder) buildAnalyze(as *ast.AnalyzeTableStmt) (Plan, error) {
	// If enable fast analyze, the storage must be tikv.Storage.
	if _, isTikvStorage := b.ctx.GetStore().(tikv.Storage); !isTikvStorage && b.ctx.GetSessionVars().EnableFastAnalyze {
		return nil, errors.Errorf("Only support fast analyze in tikv storage.")
	}
	for _, tbl := range as.TableNames {
		user := b.ctx.GetSessionVars().User
		var insertErr, selectErr error
		if user != nil {
			insertErr = ErrTableaccessDenied.GenWithStackByArgs("INSERT", user.AuthUsername, user.AuthHostname, tbl.Name.O)
			selectErr = ErrTableaccessDenied.GenWithStackByArgs("SELECT", user.AuthUsername, user.AuthHostname, tbl.Name.O)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.InsertPriv, tbl.Schema.O, tbl.Name.O, "", insertErr)
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SelectPriv, tbl.Schema.O, tbl.Name.O, "", selectErr)
	}
	opts, err := handleAnalyzeOptions(as.AnalyzeOpts)
	if err != nil {
		return nil, err
	}
	if as.IndexFlag {
		if len(as.IndexNames) == 0 {
			return b.buildAnalyzeAllIndex(as, opts)
		}
		return b.buildAnalyzeIndex(as, opts)
	}
	return b.buildAnalyzeTable(as, opts)
}

func buildShowNextRowID() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(4)
	schema.Append(buildColumnWithName("", "DB_NAME", mysql.TypeVarchar, mysql.MaxDatabaseNameLength))
	schema.Append(buildColumnWithName("", "TABLE_NAME", mysql.TypeVarchar, mysql.MaxTableNameLength))
	schema.Append(buildColumnWithName("", "COLUMN_NAME", mysql.TypeVarchar, mysql.MaxColumnNameLength))
	schema.Append(buildColumnWithName("", "NEXT_GLOBAL_ROW_ID", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "ID_TYPE", mysql.TypeVarchar, 15))
	return schema.col2Schema(), schema.names
}

func buildShowDDLFields() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(6)
	schema.Append(buildColumnWithName("", "SCHEMA_VER", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "OWNER_ID", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "OWNER_ADDRESS", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName("", "RUNNING_JOBS", mysql.TypeVarchar, 256))
	schema.Append(buildColumnWithName("", "SELF_ID", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "QUERY", mysql.TypeVarchar, 256))

	return schema.col2Schema(), schema.names
}

func buildRecoverIndexFields() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(2)
	schema.Append(buildColumnWithName("", "ADDED_COUNT", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "SCAN_COUNT", mysql.TypeLonglong, 4))
	return schema.col2Schema(), schema.names
}

func buildCleanupIndexFields() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(1)
	schema.Append(buildColumnWithName("", "REMOVED_COUNT", mysql.TypeLonglong, 4))
	return schema.col2Schema(), schema.names
}

func buildShowDDLJobsFields() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(11)
	schema.Append(buildColumnWithName("", "JOB_ID", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "DB_NAME", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "TABLE_NAME", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "JOB_TYPE", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "SCHEMA_STATE", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "SCHEMA_ID", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "TABLE_ID", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "ROW_COUNT", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "START_TIME", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName("", "END_TIME", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName("", "STATE", mysql.TypeVarchar, 64))
	return schema.col2Schema(), schema.names
}

func buildTableRegionsSchema() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(11)
	schema.Append(buildColumnWithName("", "REGION_ID", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "START_KEY", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "END_KEY", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "LEADER_ID", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "LEADER_STORE_ID", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "PEERS", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "SCATTERING", mysql.TypeTiny, 1))
	schema.Append(buildColumnWithName("", "WRITTEN_BYTES", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "READ_BYTES", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "APPROXIMATE_SIZE(MB)", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "APPROXIMATE_KEYS", mysql.TypeLonglong, 4))
	return schema.col2Schema(), schema.names
}

func buildSplitRegionsSchema() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(2)
	schema.Append(buildColumnWithName("", "TOTAL_SPLIT_REGION", mysql.TypeLonglong, 4))
	schema.Append(buildColumnWithName("", "SCATTER_FINISH_RATIO", mysql.TypeDouble, 8))
	return schema.col2Schema(), schema.names
}

func buildShowDDLJobQueriesFields() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(1)
	schema.Append(buildColumnWithName("", "QUERY", mysql.TypeVarchar, 256))
	return schema.col2Schema(), schema.names
}

func buildShowSlowSchema() (*expression.Schema, types.NameSlice) {
	longlongSize, _ := mysql.GetDefaultFieldLengthAndDecimal(mysql.TypeLonglong)
	tinySize, _ := mysql.GetDefaultFieldLengthAndDecimal(mysql.TypeTiny)
	timestampSize, _ := mysql.GetDefaultFieldLengthAndDecimal(mysql.TypeTimestamp)
	durationSize, _ := mysql.GetDefaultFieldLengthAndDecimal(mysql.TypeDuration)

	schema := newColumnsWithNames(11)
	schema.Append(buildColumnWithName("", "SQL", mysql.TypeVarchar, 4096))
	schema.Append(buildColumnWithName("", "START", mysql.TypeTimestamp, timestampSize))
	schema.Append(buildColumnWithName("", "DURATION", mysql.TypeDuration, durationSize))
	schema.Append(buildColumnWithName("", "DETAILS", mysql.TypeVarchar, 256))
	schema.Append(buildColumnWithName("", "SUCC", mysql.TypeTiny, tinySize))
	schema.Append(buildColumnWithName("", "CONN_ID", mysql.TypeLonglong, longlongSize))
	schema.Append(buildColumnWithName("", "TRANSACTION_TS", mysql.TypeLonglong, longlongSize))
	schema.Append(buildColumnWithName("", "USER", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName("", "DB", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "TABLE_IDS", mysql.TypeVarchar, 256))
	schema.Append(buildColumnWithName("", "INDEX_IDS", mysql.TypeVarchar, 256))
	schema.Append(buildColumnWithName("", "INTERNAL", mysql.TypeTiny, tinySize))
	schema.Append(buildColumnWithName("", "DIGEST", mysql.TypeVarchar, 64))
	return schema.col2Schema(), schema.names
}

func buildCancelDDLJobsFields() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(2)
	schema.Append(buildColumnWithName("", "JOB_ID", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "RESULT", mysql.TypeVarchar, 128))

	return schema.col2Schema(), schema.names
}

func buildBRIESchema() (*expression.Schema, types.NameSlice) {
	longlongSize, _ := mysql.GetDefaultFieldLengthAndDecimal(mysql.TypeLonglong)
	datetimeSize, _ := mysql.GetDefaultFieldLengthAndDecimal(mysql.TypeDatetime)

	schema := newColumnsWithNames(5)
	schema.Append(buildColumnWithName("", "Destination", mysql.TypeVarchar, 255))
	schema.Append(buildColumnWithName("", "Size", mysql.TypeLonglong, longlongSize))
	schema.Append(buildColumnWithName("", "BackupTS", mysql.TypeLonglong, longlongSize))
	schema.Append(buildColumnWithName("", "Queue Time", mysql.TypeDatetime, datetimeSize))
	schema.Append(buildColumnWithName("", "Execution Time", mysql.TypeDatetime, datetimeSize))
	return schema.col2Schema(), schema.names
}

func buildShowTelemetrySchema() (*expression.Schema, types.NameSlice) {
	schema := newColumnsWithNames(1)
	schema.Append(buildColumnWithName("", "TRACKING_ID", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName("", "LAST_STATUS", mysql.TypeString, mysql.MaxBlobWidth))
	schema.Append(buildColumnWithName("", "DATA_PREVIEW", mysql.TypeString, mysql.MaxBlobWidth))
	return schema.col2Schema(), schema.names
}

func buildColumnWithName(tableName, name string, tp byte, size int) (*expression.Column, *types.FieldName) {
	cs, cl := types.DefaultCharsetForType(tp)
	flag := mysql.UnsignedFlag
	if tp == mysql.TypeVarchar || tp == mysql.TypeBlob {
		cs = charset.CharsetUTF8MB4
		cl = charset.CollationUTF8MB4
		flag = 0
	}

	fieldType := &types.FieldType{
		Charset: cs,
		Collate: cl,
		Tp:      tp,
		Flen:    size,
		Flag:    flag,
	}
	return &expression.Column{
		RetType: fieldType,
	}, &types.FieldName{DBName: util2.InformationSchemaName, TblName: model.NewCIStr(tableName), ColName: model.NewCIStr(name)}
}

type columnsWithNames struct {
	cols  []*expression.Column
	names types.NameSlice
}

func newColumnsWithNames(cap int) *columnsWithNames {
	return &columnsWithNames{
		cols:  make([]*expression.Column, 0, 2),
		names: make(types.NameSlice, 0, 2),
	}
}

func (cwn *columnsWithNames) Append(col *expression.Column, name *types.FieldName) {
	cwn.cols = append(cwn.cols, col)
	cwn.names = append(cwn.names, name)
}

func (cwn *columnsWithNames) col2Schema() *expression.Schema {
	return expression.NewSchema(cwn.cols...)
}

// splitWhere split a where expression to a list of AND conditions.
func splitWhere(where ast.ExprNode) []ast.ExprNode {
	var conditions []ast.ExprNode
	switch x := where.(type) {
	case nil:
	case *ast.BinaryOperationExpr:
		if x.Op == opcode.LogicAnd {
			conditions = append(conditions, splitWhere(x.L)...)
			conditions = append(conditions, splitWhere(x.R)...)
		} else {
			conditions = append(conditions, x)
		}
	case *ast.ParenthesesExpr:
		conditions = append(conditions, splitWhere(x.Expr)...)
	default:
		conditions = append(conditions, where)
	}
	return conditions
}

func (b *PlanBuilder) buildShow(ctx context.Context, show *ast.ShowStmt) (Plan, error) {
	p := LogicalShow{
		ShowContents: ShowContents{
			Tp:          show.Tp,
			DBName:      show.DBName,
			Table:       show.Table,
			Column:      show.Column,
			IndexName:   show.IndexName,
			Flag:        show.Flag,
			User:        show.User,
			Roles:       show.Roles,
			Full:        show.Full,
			IfNotExists: show.IfNotExists,
			GlobalScope: show.GlobalScope,
			Extended:    show.Extended,
		},
	}.Init(b.ctx)
	isView := false
	isSequence := false
	switch show.Tp {
	case ast.ShowTables, ast.ShowTableStatus:
		if p.DBName == "" {
			return nil, ErrNoDB
		}
	case ast.ShowCreateTable, ast.ShowCreateSequence:
		user := b.ctx.GetSessionVars().User
		var err error
		if user != nil {
			err = ErrTableaccessDenied.GenWithStackByArgs("SHOW", user.AuthUsername, user.AuthHostname, show.Table.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.AllPrivMask, show.Table.Schema.L, show.Table.Name.L, "", err)
		if table, err := b.is.TableByName(show.Table.Schema, show.Table.Name); err == nil {
			isView = table.Meta().IsView()
			isSequence = table.Meta().IsSequence()
		}
	case ast.ShowCreateView:
		err := ErrSpecificAccessDenied.GenWithStackByArgs("SHOW VIEW")
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.ShowViewPriv, show.Table.Schema.L, show.Table.Name.L, "", err)
	case ast.ShowBackups, ast.ShowRestores:
		err := ErrSpecificAccessDenied.GenWithStackByArgs("SUPER")
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", err)
	case ast.ShowTableNextRowId:
		p := &ShowNextRowID{TableName: show.Table}
		p.setSchemaAndNames(buildShowNextRowID())
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SelectPriv, show.Table.Schema.L, show.Table.Name.L, "", ErrPrivilegeCheckFail)
		return p, nil
	case ast.ShowStatsBuckets, ast.ShowStatsHistograms, ast.ShowStatsMeta, ast.ShowStatsHealthy:
		user := b.ctx.GetSessionVars().User
		var err error
		if user != nil {
			err = ErrDBaccessDenied.GenWithStackByArgs(user.AuthUsername, user.AuthHostname, mysql.SystemDB)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SelectPriv, mysql.SystemDB, "", "", err)
	}
	schema, names := buildShowSchema(show, isView, isSequence)
	p.SetSchema(schema)
	p.names = names
	for _, col := range p.schema.Columns {
		col.UniqueID = b.ctx.GetSessionVars().AllocPlanColumnID()
	}
	var err error
	var np LogicalPlan
	np = p
	if show.Pattern != nil {
		show.Pattern.Expr = &ast.ColumnNameExpr{
			Name: &ast.ColumnName{Name: p.OutputNames()[0].ColName},
		}
		np, err = b.buildSelection(ctx, np, show.Pattern, nil)
		if err != nil {
			return nil, err
		}
	}
	if show.Where != nil {
		np, err = b.buildSelection(ctx, np, show.Where, nil)
		if err != nil {
			return nil, err
		}
	}
	if np != p {
		b.optFlag |= flagEliminateProjection
		fieldsLen := len(p.schema.Columns)
		proj := LogicalProjection{Exprs: make([]expression.Expression, 0, fieldsLen)}.Init(b.ctx, 0)
		schema := expression.NewSchema(make([]*expression.Column, 0, fieldsLen)...)
		for _, col := range p.schema.Columns {
			proj.Exprs = append(proj.Exprs, col)
			newCol := col.Clone().(*expression.Column)
			newCol.UniqueID = b.ctx.GetSessionVars().AllocPlanColumnID()
			schema.Append(newCol)
		}
		proj.SetSchema(schema)
		proj.SetChildren(np)
		proj.SetOutputNames(np.OutputNames())
		np = proj
	}
	if show.Tp == ast.ShowVariables || show.Tp == ast.ShowStatus {
		b.curClause = orderByClause
		orderByCol := np.Schema().Columns[0].Clone().(*expression.Column)
		sort := LogicalSort{
			ByItems: []*util.ByItems{{Expr: orderByCol}},
		}.Init(b.ctx, b.getSelectOffset())
		sort.SetChildren(np)
		np = sort
	}
	return np, nil
}

func (b *PlanBuilder) buildSimple(node ast.StmtNode) (Plan, error) {
	p := &Simple{Statement: node}

	switch raw := node.(type) {
	case *ast.FlushStmt:
		err := ErrSpecificAccessDenied.GenWithStackByArgs("RELOAD")
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.ReloadPriv, "", "", "", err)
	case *ast.AlterInstanceStmt:
		err := ErrSpecificAccessDenied.GenWithStack("ALTER INSTANCE")
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", err)
	case *ast.AlterUserStmt:
		err := ErrSpecificAccessDenied.GenWithStackByArgs("CREATE USER")
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.CreateUserPriv, "", "", "", err)
	case *ast.GrantStmt:
		if b.ctx.GetSessionVars().CurrentDB == "" && raw.Level.DBName == "" {
			if raw.Level.Level == ast.GrantLevelTable {
				return nil, ErrNoDB
			}
		}
		b.visitInfo = collectVisitInfoFromGrantStmt(b.ctx, b.visitInfo, raw)
	case *ast.BRIEStmt:
		p.setSchemaAndNames(buildBRIESchema())
		err := ErrSpecificAccessDenied.GenWithStackByArgs("SUPER")
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", err)
	case *ast.GrantRoleStmt, *ast.RevokeRoleStmt:
		err := ErrSpecificAccessDenied.GenWithStackByArgs("SUPER")
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", err)
	case *ast.RevokeStmt:
		b.visitInfo = collectVisitInfoFromRevokeStmt(b.ctx, b.visitInfo, raw)
	case *ast.KillStmt:
		// If you have the SUPER privilege, you can kill all threads and statements.
		// Otherwise, you can kill only your own threads and statements.
		sm := b.ctx.GetSessionManager()
		if sm != nil {
			if pi, ok := sm.GetProcessInfo(raw.ConnectionID); ok {
				loginUser := b.ctx.GetSessionVars().User
				if pi.User != loginUser.Username {
					b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", nil)
				}
			}
		}
	case *ast.UseStmt:
		if raw.DBName == "" {
			return nil, ErrNoDB
		}
	case *ast.ShutdownStmt:
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.ShutdownPriv, "", "", "", nil)
	case *ast.CreateStatisticsStmt:
		var selectErr, insertErr error
		user := b.ctx.GetSessionVars().User
		if user != nil {
			selectErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE STATISTICS", user.AuthUsername,
				user.AuthHostname, raw.Table.Name.L)
			insertErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE STATISTICS", user.AuthUsername,
				user.AuthHostname, "stats_extended")
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SelectPriv, raw.Table.Schema.L,
			raw.Table.Name.L, "", selectErr)
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.InsertPriv, mysql.SystemDB,
			"stats_extended", "", insertErr)
	case *ast.DropStatisticsStmt:
		var err error
		user := b.ctx.GetSessionVars().User
		if user != nil {
			err = ErrTableaccessDenied.GenWithStackByArgs("DROP STATISTICS", user.AuthUsername,
				user.AuthHostname, "stats_extended")
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.UpdatePriv, mysql.SystemDB,
			"stats_extended", "", err)
	}
	return p, nil
}

func collectVisitInfoFromRevokeStmt(sctx sessionctx.Context, vi []visitInfo, stmt *ast.RevokeStmt) []visitInfo {
	// To use REVOKE, you must have the GRANT OPTION privilege,
	// and you must have the privileges that you are granting.
	dbName := stmt.Level.DBName
	tableName := stmt.Level.TableName
	if dbName == "" {
		dbName = sctx.GetSessionVars().CurrentDB
	}
	vi = appendVisitInfo(vi, mysql.GrantPriv, dbName, tableName, "", nil)

	var allPrivs []mysql.PrivilegeType
	for _, item := range stmt.Privs {
		if item.Priv == mysql.AllPriv {
			switch stmt.Level.Level {
			case ast.GrantLevelGlobal:
				allPrivs = mysql.AllGlobalPrivs
			case ast.GrantLevelDB:
				allPrivs = mysql.AllDBPrivs
			case ast.GrantLevelTable:
				allPrivs = mysql.AllTablePrivs
			}
			break
		}
		vi = appendVisitInfo(vi, item.Priv, dbName, tableName, "", nil)
	}

	for _, priv := range allPrivs {
		vi = appendVisitInfo(vi, priv, dbName, tableName, "", nil)
	}

	return vi
}

func collectVisitInfoFromGrantStmt(sctx sessionctx.Context, vi []visitInfo, stmt *ast.GrantStmt) []visitInfo {
	// To use GRANT, you must have the GRANT OPTION privilege,
	// and you must have the privileges that you are granting.
	dbName := stmt.Level.DBName
	tableName := stmt.Level.TableName
	if dbName == "" {
		dbName = sctx.GetSessionVars().CurrentDB
	}
	vi = appendVisitInfo(vi, mysql.GrantPriv, dbName, tableName, "", nil)

	var allPrivs []mysql.PrivilegeType
	for _, item := range stmt.Privs {
		if item.Priv == mysql.AllPriv {
			switch stmt.Level.Level {
			case ast.GrantLevelGlobal:
				allPrivs = mysql.AllGlobalPrivs
			case ast.GrantLevelDB:
				allPrivs = mysql.AllDBPrivs
			case ast.GrantLevelTable:
				allPrivs = mysql.AllTablePrivs
			}
			break
		}
		vi = appendVisitInfo(vi, item.Priv, dbName, tableName, "", nil)
	}

	for _, priv := range allPrivs {
		vi = appendVisitInfo(vi, priv, dbName, tableName, "", nil)
	}

	return vi
}

func (b *PlanBuilder) getDefaultValue(col *table.Column) (*expression.Constant, error) {
	var (
		value types.Datum
		err   error
	)
	if col.DefaultIsExpr && col.DefaultExpr != nil {
		value, err = table.EvalColDefaultExpr(b.ctx, col.ToInfo(), col.DefaultExpr)
	} else {
		value, err = table.GetColDefaultValue(b.ctx, col.ToInfo())
	}
	if err != nil {
		return nil, err
	}
	return &expression.Constant{Value: value, RetType: &col.FieldType}, nil
}

func (b *PlanBuilder) findDefaultValue(cols []*table.Column, name *ast.ColumnName) (*expression.Constant, error) {
	for _, col := range cols {
		if col.Name.L == name.Name.L {
			return b.getDefaultValue(col)
		}
	}
	return nil, ErrUnknownColumn.GenWithStackByArgs(name.Name.O, "field_list")
}

// resolveGeneratedColumns resolves generated columns with their generation
// expressions respectively. onDups indicates which columns are in on-duplicate list.
func (b *PlanBuilder) resolveGeneratedColumns(ctx context.Context, columns []*table.Column, onDups map[string]struct{}, mockPlan LogicalPlan) (igc InsertGeneratedColumns, err error) {
	for _, column := range columns {
		if !column.IsGenerated() {
			continue
		}
		columnName := &ast.ColumnName{Name: column.Name}
		columnName.SetText(column.Name.O)

		idx, err := expression.FindFieldName(mockPlan.OutputNames(), columnName)
		if err != nil {
			return igc, err
		}
		colExpr := mockPlan.Schema().Columns[idx]

		expr, _, err := b.rewrite(ctx, column.GeneratedExpr, mockPlan, nil, true)
		if err != nil {
			return igc, err
		}

		igc.Columns = append(igc.Columns, columnName)
		igc.Exprs = append(igc.Exprs, expr)
		if onDups == nil {
			continue
		}
		for dep := range column.Dependences {
			if _, ok := onDups[dep]; ok {
				assign := &expression.Assignment{Col: colExpr, ColName: column.Name, Expr: expr}
				igc.OnDuplicates = append(igc.OnDuplicates, assign)
				break
			}
		}
	}
	return igc, nil
}

func (b *PlanBuilder) buildInsert(ctx context.Context, insert *ast.InsertStmt) (Plan, error) {
	ts, ok := insert.Table.TableRefs.Left.(*ast.TableSource)
	if !ok {
		return nil, infoschema.ErrTableNotExists.GenWithStackByArgs()
	}
	tn, ok := ts.Source.(*ast.TableName)
	if !ok {
		return nil, infoschema.ErrTableNotExists.GenWithStackByArgs()
	}
	tableInfo := tn.TableInfo
	if tableInfo.IsView() {
		err := errors.Errorf("insert into view %s is not supported now.", tableInfo.Name.O)
		if insert.IsReplace {
			err = errors.Errorf("replace into view %s is not supported now.", tableInfo.Name.O)
		}
		return nil, err
	}
	if tableInfo.IsSequence() {
		err := errors.Errorf("insert into sequence %s is not supported now.", tableInfo.Name.O)
		if insert.IsReplace {
			err = errors.Errorf("replace into sequence %s is not supported now.", tableInfo.Name.O)
		}
		return nil, err
	}
	// Build Schema with DBName otherwise ColumnRef with DBName cannot match any Column in Schema.
	schema, names, err := expression.TableInfo2SchemaAndNames(b.ctx, tn.Schema, tableInfo)
	if err != nil {
		return nil, err
	}
	tableInPlan, ok := b.is.TableByID(tableInfo.ID)
	if !ok {
		return nil, errors.Errorf("Can't get table %s.", tableInfo.Name.O)
	}

	insertPlan := Insert{
		Table:         tableInPlan,
		Columns:       insert.Columns,
		tableSchema:   schema,
		tableColNames: names,
		IsReplace:     insert.IsReplace,
	}.Init(b.ctx)

	if tableInfo.GetPartitionInfo() != nil && len(insert.PartitionNames) != 0 {
		givenPartitionSets := make(map[int64]struct{}, len(insert.PartitionNames))
		// check partition by name.
		for _, name := range insert.PartitionNames {
			id, err := tables.FindPartitionByName(tableInfo, name.L)
			if err != nil {
				return nil, err
			}
			givenPartitionSets[id] = struct{}{}
		}
		pt := tableInPlan.(table.PartitionedTable)
		insertPlan.Table = tables.NewPartitionTableithGivenSets(pt, givenPartitionSets)
	} else if len(insert.PartitionNames) != 0 {
		return nil, ErrPartitionClauseOnNonpartitioned
	}

	var authErr error
	if b.ctx.GetSessionVars().User != nil {
		authErr = ErrTableaccessDenied.GenWithStackByArgs("INSERT", b.ctx.GetSessionVars().User.AuthUsername,
			b.ctx.GetSessionVars().User.AuthHostname, tableInfo.Name.L)
	}

	b.visitInfo = appendVisitInfo(b.visitInfo, mysql.InsertPriv, tn.DBInfo.Name.L,
		tableInfo.Name.L, "", authErr)

	mockTablePlan := LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
	mockTablePlan.SetSchema(insertPlan.tableSchema)
	mockTablePlan.names = insertPlan.tableColNames

	checkRefColumn := func(n ast.Node) ast.Node {
		if insertPlan.NeedFillDefaultValue {
			return n
		}
		switch n.(type) {
		case *ast.ColumnName, *ast.ColumnNameExpr:
			insertPlan.NeedFillDefaultValue = true
		}
		return n
	}

	if len(insert.Setlist) > 0 {
		// Branch for `INSERT ... SET ...`.
		err := b.buildSetValuesOfInsert(ctx, insert, insertPlan, mockTablePlan, checkRefColumn)
		if err != nil {
			return nil, err
		}
	} else if len(insert.Lists) > 0 {
		// Branch for `INSERT ... VALUES ...`.
		err := b.buildValuesListOfInsert(ctx, insert, insertPlan, mockTablePlan, checkRefColumn)
		if err != nil {
			return nil, err
		}
	} else {
		// Branch for `INSERT ... SELECT ...`.
		err := b.buildSelectPlanOfInsert(ctx, insert, insertPlan)
		if err != nil {
			return nil, err
		}
	}

	mockTablePlan.SetSchema(insertPlan.Schema4OnDuplicate)
	mockTablePlan.names = insertPlan.names4OnDuplicate

	onDupColSet, err := insertPlan.resolveOnDuplicate(insert.OnDuplicate, tableInfo, func(node ast.ExprNode) (expression.Expression, error) {
		return b.rewriteInsertOnDuplicateUpdate(ctx, node, mockTablePlan, insertPlan)
	})
	if err != nil {
		return nil, err
	}

	// Calculate generated columns.
	mockTablePlan.schema = insertPlan.tableSchema
	mockTablePlan.names = insertPlan.tableColNames
	insertPlan.GenCols, err = b.resolveGeneratedColumns(ctx, insertPlan.Table.Cols(), onDupColSet, mockTablePlan)
	if err != nil {
		return nil, err
	}

	err = insertPlan.ResolveIndices()
	return insertPlan, err
}

func (p *Insert) resolveOnDuplicate(onDup []*ast.Assignment, tblInfo *model.TableInfo, yield func(ast.ExprNode) (expression.Expression, error)) (map[string]struct{}, error) {
	onDupColSet := make(map[string]struct{}, len(onDup))
	colMap := make(map[string]*table.Column, len(p.Table.Cols()))
	for _, col := range p.Table.Cols() {
		colMap[col.Name.L] = col
	}
	for _, assign := range onDup {
		// Check whether the column to be updated exists in the source table.
		idx, err := expression.FindFieldName(p.tableColNames, assign.Column)
		if err != nil {
			return nil, err
		} else if idx < 0 {
			return nil, ErrUnknownColumn.GenWithStackByArgs(assign.Column.OrigColName(), "field list")
		}

		column := colMap[assign.Column.Name.L]
		if column.Hidden {
			return nil, ErrUnknownColumn.GenWithStackByArgs(column.Name, clauseMsg[fieldList])
		}
		// Check whether the column to be updated is the generated column.
		defaultExpr := extractDefaultExpr(assign.Expr)
		if defaultExpr != nil {
			defaultExpr.Name = assign.Column
		}
		// Note: For INSERT, REPLACE, and UPDATE, if a generated column is inserted into, replaced, or updated explicitly, the only permitted value is DEFAULT.
		// see https://dev.mysql.com/doc/refman/8.0/en/create-table-generated-columns.html
		if column.IsGenerated() {
			if defaultExpr != nil {
				continue
			}
			return nil, ErrBadGeneratedColumn.GenWithStackByArgs(assign.Column.Name.O, tblInfo.Name.O)
		}

		onDupColSet[column.Name.L] = struct{}{}

		expr, err := yield(assign.Expr)
		if err != nil {
			return nil, err
		}

		p.OnDuplicate = append(p.OnDuplicate, &expression.Assignment{
			Col:     p.tableSchema.Columns[idx],
			ColName: p.tableColNames[idx].ColName,
			Expr:    expr,
		})
	}
	return onDupColSet, nil
}

func (b *PlanBuilder) getAffectCols(insertStmt *ast.InsertStmt, insertPlan *Insert) (affectedValuesCols []*table.Column, err error) {
	if len(insertStmt.Columns) > 0 {
		// This branch is for the following scenarios:
		// 1. `INSERT INTO tbl_name (col_name [, col_name] ...) {VALUES | VALUE} (value_list) [, (value_list)] ...`,
		// 2. `INSERT INTO tbl_name (col_name [, col_name] ...) SELECT ...`.
		colName := make([]string, 0, len(insertStmt.Columns))
		for _, col := range insertStmt.Columns {
			colName = append(colName, col.Name.O)
		}
		var missingColName string
		affectedValuesCols, missingColName = table.FindCols(insertPlan.Table.VisibleCols(), colName, insertPlan.Table.Meta().PKIsHandle)
		if missingColName != "" {
			return nil, ErrUnknownColumn.GenWithStackByArgs(missingColName, clauseMsg[fieldList])
		}
	} else if len(insertStmt.Setlist) == 0 {
		// This branch is for the following scenarios:
		// 1. `INSERT INTO tbl_name {VALUES | VALUE} (value_list) [, (value_list)] ...`,
		// 2. `INSERT INTO tbl_name SELECT ...`.
		affectedValuesCols = insertPlan.Table.VisibleCols()
	}
	return affectedValuesCols, nil
}

func (b *PlanBuilder) buildSetValuesOfInsert(ctx context.Context, insert *ast.InsertStmt, insertPlan *Insert, mockTablePlan *LogicalTableDual, checkRefColumn func(n ast.Node) ast.Node) error {
	tableInfo := insertPlan.Table.Meta()
	colNames := make([]string, 0, len(insert.Setlist))
	exprCols := make([]*expression.Column, 0, len(insert.Setlist))
	for _, assign := range insert.Setlist {
		idx, err := expression.FindFieldName(insertPlan.tableColNames, assign.Column)
		if err != nil {
			return err
		}
		if idx < 0 {
			return errors.Errorf("Can't find column %s", assign.Column)
		}
		colNames = append(colNames, assign.Column.Name.L)
		exprCols = append(exprCols, insertPlan.tableSchema.Columns[idx])
	}

	// Check whether the column to be updated is the generated column.
	tCols, missingColName := table.FindCols(insertPlan.Table.VisibleCols(), colNames, tableInfo.PKIsHandle)
	if missingColName != "" {
		return ErrUnknownColumn.GenWithStackByArgs(missingColName, clauseMsg[fieldList])
	}
	generatedColumns := make(map[string]struct{}, len(tCols))
	for _, tCol := range tCols {
		if tCol.IsGenerated() {
			generatedColumns[tCol.Name.L] = struct{}{}
		}
	}

	insertPlan.AllAssignmentsAreConstant = true
	for i, assign := range insert.Setlist {
		defaultExpr := extractDefaultExpr(assign.Expr)
		if defaultExpr != nil {
			defaultExpr.Name = assign.Column
		}
		// Note: For INSERT, REPLACE, and UPDATE, if a generated column is inserted into, replaced, or updated explicitly, the only permitted value is DEFAULT.
		// see https://dev.mysql.com/doc/refman/8.0/en/create-table-generated-columns.html
		if _, ok := generatedColumns[assign.Column.Name.L]; ok {
			if defaultExpr != nil {
				continue
			}
			return ErrBadGeneratedColumn.GenWithStackByArgs(assign.Column.Name.O, tableInfo.Name.O)
		}
		b.curClause = fieldList
		// subquery in insert values should not reference upper scope
		usingPlan := mockTablePlan
		if _, ok := assign.Expr.(*ast.SubqueryExpr); ok {
			usingPlan = LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
		}
		expr, _, err := b.rewriteWithPreprocess(ctx, assign.Expr, usingPlan, nil, nil, true, checkRefColumn)
		if err != nil {
			return err
		}
		if insertPlan.AllAssignmentsAreConstant {
			_, isConstant := expr.(*expression.Constant)
			insertPlan.AllAssignmentsAreConstant = isConstant
		}

		insertPlan.SetList = append(insertPlan.SetList, &expression.Assignment{
			Col:     exprCols[i],
			ColName: model.NewCIStr(colNames[i]),
			Expr:    expr,
		})
	}
	insertPlan.Schema4OnDuplicate = insertPlan.tableSchema
	insertPlan.names4OnDuplicate = insertPlan.tableColNames
	return nil
}

func (b *PlanBuilder) buildValuesListOfInsert(ctx context.Context, insert *ast.InsertStmt, insertPlan *Insert, mockTablePlan *LogicalTableDual, checkRefColumn func(n ast.Node) ast.Node) error {
	affectedValuesCols, err := b.getAffectCols(insert, insertPlan)
	if err != nil {
		return err
	}

	// If value_list and col_list are empty and we have a generated column, we can still write data to this table.
	// For example, insert into t values(); can be executed successfully if t has a generated column.
	if len(insert.Columns) > 0 || len(insert.Lists[0]) > 0 {
		// If value_list or col_list is not empty, the length of value_list should be the same with that of col_list.
		if len(insert.Lists[0]) != len(affectedValuesCols) {
			return ErrWrongValueCountOnRow.GenWithStackByArgs(1)
		}
	}

	insertPlan.AllAssignmentsAreConstant = true
	totalTableCols := insertPlan.Table.Cols()
	for i, valuesItem := range insert.Lists {
		// The length of all the value_list should be the same.
		// "insert into t values (), ()" is valid.
		// "insert into t values (), (1)" is not valid.
		// "insert into t values (1), ()" is not valid.
		// "insert into t values (1,2), (1)" is not valid.
		if i > 0 && len(insert.Lists[i-1]) != len(insert.Lists[i]) {
			return ErrWrongValueCountOnRow.GenWithStackByArgs(i + 1)
		}
		exprList := make([]expression.Expression, 0, len(valuesItem))
		for j, valueItem := range valuesItem {
			var expr expression.Expression
			var err error
			var generatedColumnWithDefaultExpr bool
			col := affectedValuesCols[j]
			switch x := valueItem.(type) {
			case *ast.DefaultExpr:
				if col.IsGenerated() {
					if x.Name != nil {
						return ErrBadGeneratedColumn.GenWithStackByArgs(col.Name.O, insertPlan.Table.Meta().Name.O)
					}
					generatedColumnWithDefaultExpr = true
					break
				}
				if x.Name != nil {
					expr, err = b.findDefaultValue(totalTableCols, x.Name)
				} else {
					expr, err = b.getDefaultValue(affectedValuesCols[j])
				}
			case *driver.ValueExpr:
				expr = &expression.Constant{
					Value:   x.Datum,
					RetType: &x.Type,
				}
			default:
				b.curClause = fieldList
				// subquery in insert values should not reference upper scope
				usingPlan := mockTablePlan
				if _, ok := valueItem.(*ast.SubqueryExpr); ok {
					usingPlan = LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
				}
				expr, _, err = b.rewriteWithPreprocess(ctx, valueItem, usingPlan, nil, nil, true, checkRefColumn)
			}
			if err != nil {
				return err
			}
			if insertPlan.AllAssignmentsAreConstant {
				_, isConstant := expr.(*expression.Constant)
				insertPlan.AllAssignmentsAreConstant = isConstant
			}
			// Note: For INSERT, REPLACE, and UPDATE, if a generated column is inserted into, replaced, or updated explicitly, the only permitted value is DEFAULT.
			// see https://dev.mysql.com/doc/refman/8.0/en/create-table-generated-columns.html
			if col.IsGenerated() {
				if generatedColumnWithDefaultExpr {
					continue
				}
				return ErrBadGeneratedColumn.GenWithStackByArgs(col.Name.O, insertPlan.Table.Meta().Name.O)
			}
			exprList = append(exprList, expr)
		}
		insertPlan.Lists = append(insertPlan.Lists, exprList)
	}
	insertPlan.Schema4OnDuplicate = insertPlan.tableSchema
	insertPlan.names4OnDuplicate = insertPlan.tableColNames
	return nil
}

func (b *PlanBuilder) buildSelectPlanOfInsert(ctx context.Context, insert *ast.InsertStmt, insertPlan *Insert) error {
	affectedValuesCols, err := b.getAffectCols(insert, insertPlan)
	if err != nil {
		return err
	}
	selectPlan, err := b.Build(ctx, insert.Select)
	if err != nil {
		return err
	}

	// Check to guarantee that the length of the row returned by select is equal to that of affectedValuesCols.
	if selectPlan.Schema().Len() != len(affectedValuesCols) {
		return ErrWrongValueCountOnRow.GenWithStackByArgs(1)
	}

	// Check to guarantee that there's no generated column.
	// This check should be done after the above one to make its behavior compatible with MySQL.
	// For example, table t has two columns, namely a and b, and b is a generated column.
	// "insert into t (b) select * from t" will raise an error that the column count is not matched.
	// "insert into t select * from t" will raise an error that there's a generated column in the column list.
	// If we do this check before the above one, "insert into t (b) select * from t" will raise an error
	// that there's a generated column in the column list.
	for _, col := range affectedValuesCols {
		if col.IsGenerated() {
			return ErrBadGeneratedColumn.GenWithStackByArgs(col.Name.O, insertPlan.Table.Meta().Name.O)
		}
	}

	names := selectPlan.OutputNames()
	insertPlan.SelectPlan, _, err = DoOptimize(ctx, b.ctx, b.optFlag, selectPlan.(LogicalPlan))
	if err != nil {
		return err
	}

	// schema4NewRow is the schema for the newly created data record based on
	// the result of the select statement.
	schema4NewRow := expression.NewSchema(make([]*expression.Column, len(insertPlan.Table.Cols()))...)
	names4NewRow := make(types.NameSlice, len(insertPlan.Table.Cols()))
	// TODO: don't clone it.
	for i, selCol := range insertPlan.SelectPlan.Schema().Columns {
		ordinal := affectedValuesCols[i].Offset
		schema4NewRow.Columns[ordinal] = &expression.Column{}
		*schema4NewRow.Columns[ordinal] = *selCol

		schema4NewRow.Columns[ordinal].RetType = &types.FieldType{}
		*schema4NewRow.Columns[ordinal].RetType = affectedValuesCols[i].FieldType

		names4NewRow[ordinal] = names[i]
	}
	for i := range schema4NewRow.Columns {
		if schema4NewRow.Columns[i] == nil {
			schema4NewRow.Columns[i] = &expression.Column{UniqueID: insertPlan.ctx.GetSessionVars().AllocPlanColumnID()}
			names4NewRow[i] = types.EmptyName
		}
	}
	insertPlan.Schema4OnDuplicate = expression.MergeSchema(insertPlan.tableSchema, schema4NewRow)
	insertPlan.names4OnDuplicate = append(insertPlan.tableColNames.Shallow(), names4NewRow...)
	return nil
}

func (b *PlanBuilder) buildLoadData(ctx context.Context, ld *ast.LoadDataStmt) (Plan, error) {
	p := &LoadData{
		IsLocal:            ld.IsLocal,
		OnDuplicate:        ld.OnDuplicate,
		Path:               ld.Path,
		Table:              ld.Table,
		Columns:            ld.Columns,
		FieldsInfo:         ld.FieldsInfo,
		LinesInfo:          ld.LinesInfo,
		IgnoreLines:        ld.IgnoreLines,
		ColumnAssignments:  ld.ColumnAssignments,
		ColumnsAndUserVars: ld.ColumnsAndUserVars,
	}
	user := b.ctx.GetSessionVars().User
	var insertErr error
	if user != nil {
		insertErr = ErrTableaccessDenied.GenWithStackByArgs("INSERT", user.AuthUsername, user.AuthHostname, p.Table.Name.O)
	}
	b.visitInfo = appendVisitInfo(b.visitInfo, mysql.InsertPriv, p.Table.Schema.O, p.Table.Name.O, "", insertErr)
	tableInfo := p.Table.TableInfo
	tableInPlan, ok := b.is.TableByID(tableInfo.ID)
	if !ok {
		db := b.ctx.GetSessionVars().CurrentDB
		return nil, infoschema.ErrTableNotExists.GenWithStackByArgs(db, tableInfo.Name.O)
	}
	schema, names, err := expression.TableInfo2SchemaAndNames(b.ctx, model.NewCIStr(""), tableInfo)
	if err != nil {
		return nil, err
	}
	mockTablePlan := LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
	mockTablePlan.SetSchema(schema)
	mockTablePlan.names = names

	p.GenCols, err = b.resolveGeneratedColumns(ctx, tableInPlan.Cols(), nil, mockTablePlan)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (b *PlanBuilder) buildLoadStats(ld *ast.LoadStatsStmt) Plan {
	p := &LoadStats{Path: ld.Path}
	return p
}

func (b *PlanBuilder) buildIndexAdvise(node *ast.IndexAdviseStmt) Plan {
	p := &IndexAdvise{
		IsLocal:     node.IsLocal,
		Path:        node.Path,
		MaxMinutes:  node.MaxMinutes,
		MaxIndexNum: node.MaxIndexNum,
		LinesInfo:   node.LinesInfo,
	}
	return p
}

func (b *PlanBuilder) buildSplitRegion(node *ast.SplitRegionStmt) (Plan, error) {
	if node.SplitSyntaxOpt != nil && node.SplitSyntaxOpt.HasPartition && node.Table.TableInfo.Partition == nil {
		return nil, ErrPartitionClauseOnNonpartitioned
	}
	if len(node.IndexName.L) != 0 {
		return b.buildSplitIndexRegion(node)
	}
	return b.buildSplitTableRegion(node)
}

func (b *PlanBuilder) buildSplitIndexRegion(node *ast.SplitRegionStmt) (Plan, error) {
	tblInfo := node.Table.TableInfo
	indexInfo := tblInfo.FindIndexByName(node.IndexName.L)
	if indexInfo == nil {
		return nil, ErrKeyDoesNotExist.GenWithStackByArgs(node.IndexName, tblInfo.Name)
	}
	mockTablePlan := LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
	schema, names, err := expression.TableInfo2SchemaAndNames(b.ctx, node.Table.Schema, tblInfo)
	if err != nil {
		return nil, err
	}
	mockTablePlan.SetSchema(schema)
	mockTablePlan.names = names

	p := &SplitRegion{
		TableInfo:      tblInfo,
		PartitionNames: node.PartitionNames,
		IndexInfo:      indexInfo,
	}
	p.names = names
	p.setSchemaAndNames(buildSplitRegionsSchema())
	// Split index regions by user specified value lists.
	if len(node.SplitOpt.ValueLists) > 0 {
		indexValues := make([][]types.Datum, 0, len(node.SplitOpt.ValueLists))
		for i, valuesItem := range node.SplitOpt.ValueLists {
			if len(valuesItem) > len(indexInfo.Columns) {
				return nil, ErrWrongValueCountOnRow.GenWithStackByArgs(i + 1)
			}
			values, err := b.convertValue2ColumnType(valuesItem, mockTablePlan, indexInfo, tblInfo)
			if err != nil {
				return nil, err
			}
			indexValues = append(indexValues, values)
		}
		p.ValueLists = indexValues
		return p, nil
	}

	// Split index regions by lower, upper value.
	checkLowerUpperValue := func(valuesItem []ast.ExprNode, name string) ([]types.Datum, error) {
		if len(valuesItem) == 0 {
			return nil, errors.Errorf("Split index `%v` region %s value count should more than 0", indexInfo.Name, name)
		}
		if len(valuesItem) > len(indexInfo.Columns) {
			return nil, errors.Errorf("Split index `%v` region column count doesn't match value count at %v", indexInfo.Name, name)
		}
		return b.convertValue2ColumnType(valuesItem, mockTablePlan, indexInfo, tblInfo)
	}
	lowerValues, err := checkLowerUpperValue(node.SplitOpt.Lower, "lower")
	if err != nil {
		return nil, err
	}
	upperValues, err := checkLowerUpperValue(node.SplitOpt.Upper, "upper")
	if err != nil {
		return nil, err
	}
	p.Lower = lowerValues
	p.Upper = upperValues

	maxSplitRegionNum := int64(config.GetGlobalConfig().SplitRegionMaxNum)
	if node.SplitOpt.Num > maxSplitRegionNum {
		return nil, errors.Errorf("Split index region num exceeded the limit %v", maxSplitRegionNum)
	} else if node.SplitOpt.Num < 1 {
		return nil, errors.Errorf("Split index region num should more than 0")
	}
	p.Num = int(node.SplitOpt.Num)
	return p, nil
}

func (b *PlanBuilder) convertValue2ColumnType(valuesItem []ast.ExprNode, mockTablePlan LogicalPlan, indexInfo *model.IndexInfo, tblInfo *model.TableInfo) ([]types.Datum, error) {
	values := make([]types.Datum, 0, len(valuesItem))
	for j, valueItem := range valuesItem {
		colOffset := indexInfo.Columns[j].Offset
		value, err := b.convertValue(valueItem, mockTablePlan, tblInfo.Columns[colOffset])
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (b *PlanBuilder) convertValue(valueItem ast.ExprNode, mockTablePlan LogicalPlan, col *model.ColumnInfo) (d types.Datum, err error) {
	var expr expression.Expression
	switch x := valueItem.(type) {
	case *driver.ValueExpr:
		expr = &expression.Constant{
			Value:   x.Datum,
			RetType: &x.Type,
		}
	default:
		expr, _, err = b.rewrite(context.TODO(), valueItem, mockTablePlan, nil, true)
		if err != nil {
			return d, err
		}
	}
	constant, ok := expr.(*expression.Constant)
	if !ok {
		return d, errors.New("Expect constant values")
	}
	value, err := constant.Eval(chunk.Row{})
	if err != nil {
		return d, err
	}
	d, err = value.ConvertTo(b.ctx.GetSessionVars().StmtCtx, &col.FieldType)
	if err != nil {
		if !types.ErrTruncated.Equal(err) && !types.ErrTruncatedWrongVal.Equal(err) {
			return d, err
		}
		valStr, err1 := value.ToString()
		if err1 != nil {
			return d, err
		}
		return d, types.ErrTruncated.GenWithStack("Incorrect value: '%-.128s' for column '%.192s'", valStr, col.Name.O)
	}
	return d, nil
}

func (b *PlanBuilder) buildSplitTableRegion(node *ast.SplitRegionStmt) (Plan, error) {
	tblInfo := node.Table.TableInfo
	handleColInfos := buildHandleColumnInfos(tblInfo)
	mockTablePlan := LogicalTableDual{}.Init(b.ctx, b.getSelectOffset())
	schema, names, err := expression.TableInfo2SchemaAndNames(b.ctx, node.Table.Schema, tblInfo)
	if err != nil {
		return nil, err
	}
	mockTablePlan.SetSchema(schema)
	mockTablePlan.names = names

	p := &SplitRegion{
		TableInfo:      tblInfo,
		PartitionNames: node.PartitionNames,
	}
	p.setSchemaAndNames(buildSplitRegionsSchema())
	if len(node.SplitOpt.ValueLists) > 0 {
		values := make([][]types.Datum, 0, len(node.SplitOpt.ValueLists))
		for i, valuesItem := range node.SplitOpt.ValueLists {
			data, err := convertValueListToData(valuesItem, handleColInfos, i, b, mockTablePlan)
			if err != nil {
				return nil, err
			}
			values = append(values, data)
		}
		p.ValueLists = values
		return p, nil
	}

	p.Lower, err = convertValueListToData(node.SplitOpt.Lower, handleColInfos, lowerBound, b, mockTablePlan)
	if err != nil {
		return nil, err
	}
	p.Upper, err = convertValueListToData(node.SplitOpt.Upper, handleColInfos, upperBound, b, mockTablePlan)
	if err != nil {
		return nil, err
	}

	maxSplitRegionNum := int64(config.GetGlobalConfig().SplitRegionMaxNum)
	if node.SplitOpt.Num > maxSplitRegionNum {
		return nil, errors.Errorf("Split table region num exceeded the limit %v", maxSplitRegionNum)
	} else if node.SplitOpt.Num < 1 {
		return nil, errors.Errorf("Split table region num should more than 0")
	}
	p.Num = int(node.SplitOpt.Num)
	return p, nil
}

func buildHandleColumnInfos(tblInfo *model.TableInfo) []*model.ColumnInfo {
	switch {
	case tblInfo.PKIsHandle:
		if col := tblInfo.GetPkColInfo(); col != nil {
			return []*model.ColumnInfo{col}
		}
	case tblInfo.IsCommonHandle:
		pkIdx := tables.FindPrimaryIndex(tblInfo)
		pkCols := make([]*model.ColumnInfo, 0, len(pkIdx.Columns))
		cols := tblInfo.Columns
		for _, idxCol := range pkIdx.Columns {
			pkCols = append(pkCols, cols[idxCol.Offset])
		}
		return pkCols
	default:
		return []*model.ColumnInfo{model.NewExtraHandleColInfo()}
	}
	return nil
}

const (
	lowerBound int = -1
	upperBound int = -2
)

func convertValueListToData(valueList []ast.ExprNode, handleColInfos []*model.ColumnInfo, rowIdx int,
	b *PlanBuilder, mockTablePlan *LogicalTableDual) ([]types.Datum, error) {
	if len(valueList) != len(handleColInfos) {
		var err error
		switch rowIdx {
		case lowerBound:
			err = errors.Errorf("Split table region lower value count should be %d", len(handleColInfos))
		case upperBound:
			err = errors.Errorf("Split table region upper value count should be %d", len(handleColInfos))
		default:
			err = ErrWrongValueCountOnRow.GenWithStackByArgs(rowIdx)
		}
		return nil, err
	}
	data := make([]types.Datum, 0, len(handleColInfos))
	for i, v := range valueList {
		convertedDatum, err := b.convertValue(v, mockTablePlan, handleColInfos[i])
		if err != nil {
			return nil, err
		}
		data = append(data, convertedDatum)
	}
	return data, nil
}

func (b *PlanBuilder) buildDDL(ctx context.Context, node ast.DDLNode) (Plan, error) {
	var authErr error
	switch v := node.(type) {
	case *ast.AlterDatabaseStmt:
		if v.AlterDefaultDatabase {
			v.Name = b.ctx.GetSessionVars().CurrentDB
		}
		if v.Name == "" {
			return nil, ErrNoDB
		}
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrDBaccessDenied.GenWithStackByArgs("ALTER", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Name)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.AlterPriv, v.Name, "", "", authErr)
	case *ast.AlterTableStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("ALTER", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Table.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.AlterPriv, v.Table.Schema.L,
			v.Table.Name.L, "", authErr)
		for _, spec := range v.Specs {
			if spec.Tp == ast.AlterTableRenameTable || spec.Tp == ast.AlterTableExchangePartition {
				if b.ctx.GetSessionVars().User != nil {
					authErr = ErrTableaccessDenied.GenWithStackByArgs("DROP", b.ctx.GetSessionVars().User.AuthUsername,
						b.ctx.GetSessionVars().User.AuthHostname, v.Table.Name.L)
				}
				b.visitInfo = appendVisitInfo(b.visitInfo, mysql.DropPriv, v.Table.Schema.L,
					v.Table.Name.L, "", authErr)

				if b.ctx.GetSessionVars().User != nil {
					authErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE", b.ctx.GetSessionVars().User.AuthUsername,
						b.ctx.GetSessionVars().User.AuthHostname, spec.NewTable.Name.L)
				}
				b.visitInfo = appendVisitInfo(b.visitInfo, mysql.CreatePriv, spec.NewTable.Schema.L,
					spec.NewTable.Name.L, "", authErr)

				if b.ctx.GetSessionVars().User != nil {
					authErr = ErrTableaccessDenied.GenWithStackByArgs("INSERT", b.ctx.GetSessionVars().User.AuthUsername,
						b.ctx.GetSessionVars().User.AuthHostname, spec.NewTable.Name.L)
				}
				b.visitInfo = appendVisitInfo(b.visitInfo, mysql.InsertPriv, spec.NewTable.Schema.L,
					spec.NewTable.Name.L, "", authErr)
			} else if spec.Tp == ast.AlterTableDropPartition {
				if b.ctx.GetSessionVars().User != nil {
					authErr = ErrTableaccessDenied.GenWithStackByArgs("DROP", b.ctx.GetSessionVars().User.AuthUsername,
						b.ctx.GetSessionVars().User.AuthHostname, v.Table.Name.L)
				}
				b.visitInfo = appendVisitInfo(b.visitInfo, mysql.DropPriv, v.Table.Schema.L,
					v.Table.Name.L, "", authErr)
			}
		}
	case *ast.CreateDatabaseStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrDBaccessDenied.GenWithStackByArgs(b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Name)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.CreatePriv, v.Name,
			"", "", authErr)
	case *ast.CreateIndexStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("INDEX", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Table.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.IndexPriv, v.Table.Schema.L,
			v.Table.Name.L, "", authErr)
	case *ast.CreateTableStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Table.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.CreatePriv, v.Table.Schema.L,
			v.Table.Name.L, "", authErr)
		if v.ReferTable != nil {
			if b.ctx.GetSessionVars().User != nil {
				authErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE", b.ctx.GetSessionVars().User.AuthUsername,
					b.ctx.GetSessionVars().User.AuthHostname, v.ReferTable.Name.L)
			}
			b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SelectPriv, v.ReferTable.Schema.L,
				v.ReferTable.Name.L, "", authErr)
		}
	case *ast.CreateViewStmt:
		b.capFlag |= canExpandAST
		b.capFlag |= collectUnderlyingViewName
		defer func() {
			b.capFlag &= ^canExpandAST
			b.capFlag &= ^collectUnderlyingViewName
		}()
		b.underlyingViewNames = set.NewStringSet()
		plan, err := b.Build(ctx, v.Select)
		if err != nil {
			return nil, err
		}
		if b.underlyingViewNames.Exist(v.ViewName.Schema.L + "." + v.ViewName.Name.L) {
			return nil, ErrNoSuchTable.GenWithStackByArgs(v.ViewName.Schema.O, v.ViewName.Name.O)
		}
		schema := plan.Schema()
		names := plan.OutputNames()
		if v.Cols == nil {
			adjustOverlongViewColname(plan.(LogicalPlan))
			v.Cols = make([]model.CIStr, len(schema.Columns))
			for i, name := range names {
				v.Cols[i] = name.ColName
			}
		}
		if len(v.Cols) != schema.Len() {
			return nil, ddl.ErrViewWrongList
		}
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE VIEW", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.ViewName.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.CreateViewPriv, v.ViewName.Schema.L,
			v.ViewName.Name.L, "", authErr)
		if v.Definer.CurrentUser && b.ctx.GetSessionVars().User != nil {
			v.Definer = b.ctx.GetSessionVars().User
		}
		if b.ctx.GetSessionVars().User != nil && v.Definer.String() != b.ctx.GetSessionVars().User.String() {
			err = ErrSpecificAccessDenied.GenWithStackByArgs("SUPER")
			b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "",
				"", "", err)
		}
	case *ast.CreateSequenceStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Name.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.CreatePriv, v.Name.Schema.L,
			v.Name.Name.L, "", authErr)
	case *ast.DropDatabaseStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrDBaccessDenied.GenWithStackByArgs(b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Name)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.DropPriv, v.Name,
			"", "", authErr)
	case *ast.DropIndexStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("INDEx", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Table.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.IndexPriv, v.Table.Schema.L,
			v.Table.Name.L, "", authErr)
	case *ast.DropTableStmt:
		for _, tableVal := range v.Tables {
			if b.ctx.GetSessionVars().User != nil {
				authErr = ErrTableaccessDenied.GenWithStackByArgs("DROP", b.ctx.GetSessionVars().User.AuthUsername,
					b.ctx.GetSessionVars().User.AuthHostname, tableVal.Name.L)
			}
			b.visitInfo = appendVisitInfo(b.visitInfo, mysql.DropPriv, tableVal.Schema.L,
				tableVal.Name.L, "", authErr)
		}
	case *ast.DropSequenceStmt:
		for _, sequence := range v.Sequences {
			if b.ctx.GetSessionVars().User != nil {
				authErr = ErrTableaccessDenied.GenWithStackByArgs("DROP", b.ctx.GetSessionVars().User.AuthUsername,
					b.ctx.GetSessionVars().User.AuthHostname, sequence.Name.L)
			}
			b.visitInfo = appendVisitInfo(b.visitInfo, mysql.DropPriv, sequence.Schema.L,
				sequence.Name.L, "", authErr)
		}
	case *ast.TruncateTableStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("DROP", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.Table.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.DropPriv, v.Table.Schema.L,
			v.Table.Name.L, "", authErr)
	case *ast.RenameTableStmt:
		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("ALTER", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.OldTable.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.AlterPriv, v.OldTable.Schema.L,
			v.OldTable.Name.L, "", authErr)

		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("DROP", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.OldTable.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.DropPriv, v.OldTable.Schema.L,
			v.OldTable.Name.L, "", authErr)

		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("CREATE", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.NewTable.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.CreatePriv, v.NewTable.Schema.L,
			v.NewTable.Name.L, "", authErr)

		if b.ctx.GetSessionVars().User != nil {
			authErr = ErrTableaccessDenied.GenWithStackByArgs("INSERT", b.ctx.GetSessionVars().User.AuthUsername,
				b.ctx.GetSessionVars().User.AuthHostname, v.NewTable.Name.L)
		}
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.InsertPriv, v.NewTable.Schema.L,
			v.NewTable.Name.L, "", authErr)
	case *ast.RecoverTableStmt, *ast.FlashBackTableStmt:
		// Recover table command can only be executed by administrator.
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", nil)
	case *ast.LockTablesStmt, *ast.UnlockTablesStmt:
		// TODO: add Lock Table privilege check.
	case *ast.CleanupTableLockStmt:
		// This command can only be executed by administrator.
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", nil)
	case *ast.RepairTableStmt:
		// Repair table command can only be executed by administrator.
		b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", nil)
	}
	p := &DDL{Statement: node}
	return p, nil
}

const (
	// TraceFormatRow indicates row tracing format.
	TraceFormatRow = "row"
	// TraceFormatJSON indicates json tracing format.
	TraceFormatJSON = "json"
	// TraceFormatLog indicates log tracing format.
	TraceFormatLog = "log"
)

// buildTrace builds a trace plan. Inside this method, it first optimize the
// underlying query and then constructs a schema, which will be used to constructs
// rows result.
func (b *PlanBuilder) buildTrace(trace *ast.TraceStmt) (Plan, error) {
	p := &Trace{StmtNode: trace.Stmt, Format: trace.Format}
	switch trace.Format {
	case TraceFormatRow:
		schema := newColumnsWithNames(3)
		schema.Append(buildColumnWithName("", "operation", mysql.TypeString, mysql.MaxBlobWidth))
		schema.Append(buildColumnWithName("", "startTS", mysql.TypeString, mysql.MaxBlobWidth))
		schema.Append(buildColumnWithName("", "duration", mysql.TypeString, mysql.MaxBlobWidth))
		p.SetSchema(schema.col2Schema())
		p.names = schema.names
	case TraceFormatJSON:
		schema := newColumnsWithNames(1)
		schema.Append(buildColumnWithName("", "operation", mysql.TypeString, mysql.MaxBlobWidth))
		p.SetSchema(schema.col2Schema())
		p.names = schema.names
	case TraceFormatLog:
		schema := newColumnsWithNames(4)
		schema.Append(buildColumnWithName("", "time", mysql.TypeTimestamp, mysql.MaxBlobWidth))
		schema.Append(buildColumnWithName("", "event", mysql.TypeString, mysql.MaxBlobWidth))
		schema.Append(buildColumnWithName("", "tags", mysql.TypeString, mysql.MaxBlobWidth))
		schema.Append(buildColumnWithName("", "spanName", mysql.TypeString, mysql.MaxBlobWidth))
		p.SetSchema(schema.col2Schema())
		p.names = schema.names
	default:
		return nil, errors.New("trace format should be one of 'row', 'log' or 'json'")
	}
	return p, nil
}

func (b *PlanBuilder) buildExplainPlan(targetPlan Plan, format string, rows [][]string, analyze bool, execStmt ast.StmtNode) (Plan, error) {
	p := &Explain{
		TargetPlan: targetPlan,
		Format:     format,
		Analyze:    analyze,
		ExecStmt:   execStmt,
		Rows:       rows,
	}
	p.ctx = b.ctx
	return p, p.prepareSchema()
}

// buildExplainFor gets *last* (maybe running or finished) query plan from connection #connection id.
// See https://dev.mysql.com/doc/refman/8.0/en/explain-for-connection.html.
func (b *PlanBuilder) buildExplainFor(explainFor *ast.ExplainForStmt) (Plan, error) {
	processInfo, ok := b.ctx.GetSessionManager().GetProcessInfo(explainFor.ConnectionID)
	if !ok {
		return nil, ErrNoSuchThread.GenWithStackByArgs(explainFor.ConnectionID)
	}
	if b.ctx.GetSessionVars() != nil && b.ctx.GetSessionVars().User != nil {
		if b.ctx.GetSessionVars().User.Username != processInfo.User {
			err := ErrAccessDenied.GenWithStackByArgs(b.ctx.GetSessionVars().User.Username, b.ctx.GetSessionVars().User.Hostname)
			// Different from MySQL's behavior and document.
			b.visitInfo = appendVisitInfo(b.visitInfo, mysql.SuperPriv, "", "", "", err)
		}
	}

	targetPlan, ok := processInfo.Plan.(Plan)
	if !ok || targetPlan == nil {
		return &Explain{Format: explainFor.Format}, nil
	}
	var rows [][]string
	if explainFor.Format == ast.ExplainFormatROW {
		rows = processInfo.PlanExplainRows
	}
	return b.buildExplainPlan(targetPlan, explainFor.Format, rows, false, nil)
}

func (b *PlanBuilder) buildExplain(ctx context.Context, explain *ast.ExplainStmt) (Plan, error) {
	if show, ok := explain.Stmt.(*ast.ShowStmt); ok {
		return b.buildShow(ctx, show)
	}
	targetPlan, _, err := OptimizeAstNode(ctx, b.ctx, explain.Stmt, b.is)
	if err != nil {
		return nil, err
	}

	return b.buildExplainPlan(targetPlan, explain.Format, nil, explain.Analyze, explain.Stmt)
}

func (b *PlanBuilder) buildSelectInto(ctx context.Context, sel *ast.SelectStmt) (Plan, error) {
	selectIntoInfo := sel.SelectIntoOpt
	sel.SelectIntoOpt = nil
	targetPlan, _, err := OptimizeAstNode(ctx, b.ctx, sel, b.is)
	if err != nil {
		return nil, err
	}
	b.visitInfo = appendVisitInfo(b.visitInfo, mysql.FilePriv, "", "", "", ErrSpecificAccessDenied.GenWithStackByArgs("FILE"))
	return &SelectInto{
		TargetPlan: targetPlan,
		IntoOpt:    selectIntoInfo,
	}, nil
}

func buildShowProcedureSchema() (*expression.Schema, []*types.FieldName) {
	tblName := "ROUTINES"
	schema := newColumnsWithNames(11)
	schema.Append(buildColumnWithName(tblName, "Db", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Name", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Type", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Definer", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Modified", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName(tblName, "Created", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName(tblName, "Security_type", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Comment", mysql.TypeBlob, 196605))
	schema.Append(buildColumnWithName(tblName, "character_set_client", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "collation_connection", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "Database Collation", mysql.TypeVarchar, 32))
	return schema.col2Schema(), schema.names
}

func buildShowTriggerSchema() (*expression.Schema, []*types.FieldName) {
	tblName := "TRIGGERS"
	schema := newColumnsWithNames(11)
	schema.Append(buildColumnWithName(tblName, "Trigger", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Event", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Table", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Statement", mysql.TypeBlob, 196605))
	schema.Append(buildColumnWithName(tblName, "Timing", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Created", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName(tblName, "sql_mode", mysql.TypeBlob, 8192))
	schema.Append(buildColumnWithName(tblName, "Definer", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "character_set_client", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "collation_connection", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "Database Collation", mysql.TypeVarchar, 32))
	return schema.col2Schema(), schema.names
}

func buildShowEventsSchema() (*expression.Schema, []*types.FieldName) {
	tblName := "EVENTS"
	schema := newColumnsWithNames(15)
	schema.Append(buildColumnWithName(tblName, "Db", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Name", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Time zone", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "Definer", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Type", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Execute At", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName(tblName, "Interval Value", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Interval Field", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName(tblName, "Starts", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName(tblName, "Ends", mysql.TypeDatetime, 19))
	schema.Append(buildColumnWithName(tblName, "Status", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "Originator", mysql.TypeInt24, 4))
	schema.Append(buildColumnWithName(tblName, "character_set_client", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "collation_connection", mysql.TypeVarchar, 32))
	schema.Append(buildColumnWithName(tblName, "Database Collation", mysql.TypeVarchar, 32))
	return schema.col2Schema(), schema.names
}

func buildShowWarningsSchema() (*expression.Schema, types.NameSlice) {
	tblName := "WARNINGS"
	schema := newColumnsWithNames(3)
	schema.Append(buildColumnWithName(tblName, "Level", mysql.TypeVarchar, 64))
	schema.Append(buildColumnWithName(tblName, "Code", mysql.TypeLong, 19))
	schema.Append(buildColumnWithName(tblName, "Message", mysql.TypeVarchar, 64))
	return schema.col2Schema(), schema.names
}

// buildShowSchema builds column info for ShowStmt including column name and type.
func buildShowSchema(s *ast.ShowStmt, isView bool, isSequence bool) (schema *expression.Schema, outputNames []*types.FieldName) {
	var names []string
	var ftypes []byte
	switch s.Tp {
	case ast.ShowProcedureStatus:
		return buildShowProcedureSchema()
	case ast.ShowTriggers:
		return buildShowTriggerSchema()
	case ast.ShowEvents:
		return buildShowEventsSchema()
	case ast.ShowWarnings, ast.ShowErrors:
		return buildShowWarningsSchema()
	case ast.ShowRegions:
		return buildTableRegionsSchema()
	case ast.ShowEngines:
		names = []string{"Engine", "Support", "Comment", "Transactions", "XA", "Savepoints"}
	case ast.ShowConfig:
		names = []string{"Type", "Instance", "Name", "Value"}
	case ast.ShowDatabases:
		names = []string{"Database"}
	case ast.ShowOpenTables:
		names = []string{"Database", "Table", "In_use", "Name_locked"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLong, mysql.TypeLong}
	case ast.ShowTables:
		names = []string{fmt.Sprintf("Tables_in_%s", s.DBName)}
		if s.Full {
			names = append(names, "Table_type")
		}
	case ast.ShowTableStatus:
		names = []string{"Name", "Engine", "Version", "Row_format", "Rows", "Avg_row_length",
			"Data_length", "Max_data_length", "Index_length", "Data_free", "Auto_increment",
			"Create_time", "Update_time", "Check_time", "Collation", "Checksum",
			"Create_options", "Comment"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeLonglong,
			mysql.TypeLonglong, mysql.TypeLonglong, mysql.TypeLonglong, mysql.TypeLonglong, mysql.TypeLonglong,
			mysql.TypeDatetime, mysql.TypeDatetime, mysql.TypeDatetime, mysql.TypeVarchar, mysql.TypeVarchar,
			mysql.TypeVarchar, mysql.TypeVarchar}
	case ast.ShowColumns:
		names = table.ColDescFieldNames(s.Full)
	case ast.ShowCharset:
		names = []string{"Charset", "Description", "Default collation", "Maxlen"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong}
	case ast.ShowVariables, ast.ShowStatus:
		names = []string{"Variable_name", "Value"}
	case ast.ShowCollation:
		names = []string{"Collation", "Charset", "Id", "Default", "Compiled", "Sortlen"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong,
			mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong}
	case ast.ShowCreateTable, ast.ShowCreateSequence:
		if isSequence {
			names = []string{"Sequence", "Create Sequence"}
		} else if isView {
			names = []string{"View", "Create View", "character_set_client", "collation_connection"}
		} else {
			names = []string{"Table", "Create Table"}
		}
	case ast.ShowCreateUser:
		if s.User != nil {
			names = []string{fmt.Sprintf("CREATE USER for %s", s.User)}
		}
	case ast.ShowCreateView:
		names = []string{"View", "Create View", "character_set_client", "collation_connection"}
	case ast.ShowCreateDatabase:
		names = []string{"Database", "Create Database"}
	case ast.ShowDrainerStatus:
		names = []string{"NodeID", "Address", "State", "Max_Commit_Ts", "Update_Time"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeVarchar}
	case ast.ShowGrants:
		if s.User != nil {
			names = []string{fmt.Sprintf("Grants for %s", s.User)}
		} else {
			// Don't know the name yet, so just say "user"
			names = []string{"Grants for User"}
		}
	case ast.ShowIndex:
		names = []string{"Table", "Non_unique", "Key_name", "Seq_in_index",
			"Column_name", "Collation", "Cardinality", "Sub_part", "Packed",
			"Null", "Index_type", "Comment", "Index_comment", "Visible", "Expression"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeVarchar, mysql.TypeLonglong,
			mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeLonglong,
			mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar,
			mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar}
	case ast.ShowPlugins:
		names = []string{"Name", "Status", "Type", "Library", "License", "Version"}
		ftypes = []byte{
			mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar,
		}
	case ast.ShowProcessList:
		names = []string{"Id", "User", "Host", "db", "Command", "Time", "State", "Info"}
		ftypes = []byte{mysql.TypeLonglong, mysql.TypeVarchar, mysql.TypeVarchar,
			mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLong, mysql.TypeVarchar, mysql.TypeString}
	case ast.ShowPumpStatus:
		names = []string{"NodeID", "Address", "State", "Max_Commit_Ts", "Update_Time"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeVarchar}
	case ast.ShowStatsMeta:
		names = []string{"Db_name", "Table_name", "Partition_name", "Update_time", "Modify_count", "Row_count"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeDatetime, mysql.TypeLonglong, mysql.TypeLonglong}
	case ast.ShowStatsHistograms:
		names = []string{"Db_name", "Table_name", "Partition_name", "Column_name", "Is_index", "Update_time", "Distinct_count", "Null_count", "Avg_col_size", "Correlation"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeTiny, mysql.TypeDatetime,
			mysql.TypeLonglong, mysql.TypeLonglong, mysql.TypeDouble, mysql.TypeDouble}
	case ast.ShowStatsBuckets:
		names = []string{"Db_name", "Table_name", "Partition_name", "Column_name", "Is_index", "Bucket_id", "Count",
			"Repeats", "Lower_Bound", "Upper_Bound"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeTiny, mysql.TypeLonglong,
			mysql.TypeLonglong, mysql.TypeLonglong, mysql.TypeVarchar, mysql.TypeVarchar}
	case ast.ShowStatsHealthy:
		names = []string{"Db_name", "Table_name", "Partition_name", "Healthy"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong}
	case ast.ShowProfiles: // ShowProfiles is deprecated.
		names = []string{"Query_ID", "Duration", "Query"}
		ftypes = []byte{mysql.TypeLong, mysql.TypeDouble, mysql.TypeVarchar}
	case ast.ShowMasterStatus:
		names = []string{"File", "Position", "Binlog_Do_DB", "Binlog_Ignore_DB", "Executed_Gtid_Set"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar}
	case ast.ShowPrivileges:
		names = []string{"Privilege", "Context", "Comment"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar}
	case ast.ShowBindings:
		names = []string{"Original_sql", "Bind_sql", "Default_db", "Status", "Create_time", "Update_time", "Charset", "Collation", "Source"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeDatetime, mysql.TypeDatetime, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar}
	case ast.ShowAnalyzeStatus:
		names = []string{"Table_schema", "Table_name", "Partition_name", "Job_info", "Processed_rows", "Start_time", "State"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeLonglong, mysql.TypeDatetime, mysql.TypeVarchar}
	case ast.ShowBuiltins:
		names = []string{"Supported_builtin_functions"}
		ftypes = []byte{mysql.TypeVarchar}
	case ast.ShowBackups, ast.ShowRestores:
		names = []string{"Destination", "State", "Progress", "Queue_time", "Execution_time", "Finish_time", "Connection"}
		ftypes = []byte{mysql.TypeVarchar, mysql.TypeVarchar, mysql.TypeDouble, mysql.TypeDatetime, mysql.TypeDatetime, mysql.TypeDatetime, mysql.TypeLonglong}
	}

	schema = expression.NewSchema(make([]*expression.Column, 0, len(names))...)
	outputNames = make([]*types.FieldName, 0, len(names))
	for i := range names {
		col := &expression.Column{}
		outputNames = append(outputNames, &types.FieldName{ColName: model.NewCIStr(names[i])})
		// User varchar as the default return column type.
		tp := mysql.TypeVarchar
		if len(ftypes) != 0 && ftypes[i] != mysql.TypeUnspecified {
			tp = ftypes[i]
		}
		fieldType := types.NewFieldType(tp)
		fieldType.Flen, fieldType.Decimal = mysql.GetDefaultFieldLengthAndDecimal(tp)
		fieldType.Charset, fieldType.Collate = types.DefaultCharsetForType(tp)
		col.RetType = fieldType
		schema.Append(col)
	}
	return
}

func buildChecksumTableSchema() (*expression.Schema, []*types.FieldName) {
	schema := newColumnsWithNames(5)
	schema.Append(buildColumnWithName("", "Db_name", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName("", "Table_name", mysql.TypeVarchar, 128))
	schema.Append(buildColumnWithName("", "Checksum_crc64_xor", mysql.TypeLonglong, 22))
	schema.Append(buildColumnWithName("", "Total_kvs", mysql.TypeLonglong, 22))
	schema.Append(buildColumnWithName("", "Total_bytes", mysql.TypeLonglong, 22))
	return schema.col2Schema(), schema.names
}

// adjustOverlongViewColname adjusts the overlong outputNames of a view to
// `new_exp_$off` where `$off` is the offset of the output column, $off starts from 1.
// There is still some MySQL compatible problems.
func adjustOverlongViewColname(plan LogicalPlan) {
	outputNames := plan.OutputNames()
	for i := range outputNames {
		if outputName := outputNames[i].ColName.L; len(outputName) > mysql.MaxColumnNameLength {
			outputNames[i].ColName = model.NewCIStr(fmt.Sprintf("name_exp_%d", i+1))
		}
	}
}
