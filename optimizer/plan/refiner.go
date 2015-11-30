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

package plan

import (
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/parser/opcode"
)

func refine(p Plan) {
	r := refiner{}
	p.Accept(&r)
}

// refiner tries to build index range, bypass sort, set limit for source plan.
// It prepares the plan for cost estimation.
type refiner struct {
	conditions []ast.ExprNode
	// store index scan plan for sort to use.
	indexScan *IndexScan
}

func (r *refiner) Enter(in Plan) (Plan, bool) {
	switch x := in.(type) {
	case *Filter:
		r.conditions = x.Conditions
	case *IndexScan:
		r.indexScan = x
	}
	return in, false
}

func (r *refiner) Leave(in Plan) (Plan, bool) {
	switch x := in.(type) {
	case *IndexScan:
		r.buildIndexRange(x)
	case *Sort:
		r.sortBypass(x)
	case *Limit:
		x.SetLimit(0)
	}
	return in, true
}

func (r *refiner) sortBypass(p *Sort) {
	if r.indexScan != nil {
		idx := r.indexScan.Index
		if len(p.ByItems) > len(idx.Columns) {
			return
		}
		var desc bool
		for i, val := range p.ByItems {
			if val.Desc {
				desc = true
			}
			cn, ok := val.Expr.(*ast.ColumnNameExpr)
			if !ok {
				return
			}
			if r.indexScan.Table.Name.L != cn.Refer.Table.Name.L {
				return
			}
			indexColumn := idx.Columns[i]
			if indexColumn.Name.L != cn.Refer.Column.Name.L {
				return
			}
		}
		if desc {
			// TODO: support desc when index reverse iterator is supported.
			r.indexScan.Desc = true
			return
		}
		p.Bypass = true
	}
}

var fullRange = []rangePoint{
	{start: true},
	{value: MaxVal},
}

func (r *refiner) buildIndexRange(p *IndexScan) {
	rb := rangeBuilder{}
	for i := 0; i < len(p.Index.Columns); i++ {
		checker := conditionChecker{idx: p.Index, tableName: p.Table.Name, columnOffset: i}
		rangePoints := fullRange
		var changed bool
		for _, cond := range r.conditions {
			if checker.check(cond) {
				rangePoints = rb.intersection(rangePoints, rb.build(cond))
				changed = true
			}
		}
		if !changed {
			break
		}
		if i == 0 {
			p.Ranges = rb.buildIndexRanges(rangePoints)
		} else {
			p.Ranges = rb.appendIndexRanges(p.Ranges, rangePoints)
		}
	}
}

// conditionChecker checks if this condition can be pushed to index plan.
type conditionChecker struct {
	tableName model.CIStr
	idx       *model.IndexInfo
	// the offset of the indexed column to be checked.
	columnOffset int
}

func (c *conditionChecker) check(condition ast.ExprNode) bool {
	switch x := condition.(type) {
	case *ast.BinaryOperationExpr:
		return c.checkBinaryOperation(x)
	case *ast.ParenthesesExpr:
		return c.check(x.Expr)
	case *ast.PatternInExpr:
		if x.Sel != nil || x.Not {
			return false
		}
		if !c.checkColumnExpr(x.Expr) {
			return false
		}
		for _, val := range x.List {
			if !val.IsStatic() {
				return false
			}
		}
		return true
	case *ast.PatternLikeExpr:
		if x.Not {
			return false
		}
		if !c.checkColumnExpr(x.Expr) {
			return false
		}
		if !x.Pattern.IsStatic() {
			return false
		}
		patternStr := x.Pattern.GetValue().(string)
		firstChar := patternStr[0]
		return firstChar != '%' && firstChar != '.'
	case *ast.BetweenExpr:
		if !c.checkColumnExpr(x.Expr) {
			return false
		}
		if !x.Left.IsStatic() || !x.Right.IsStatic() {
			return false
		}
	case *ast.IsNullExpr:
		if !c.checkColumnExpr(x.Expr) {
			return false
		}
	case *ast.IsTruthExpr:
		if !c.checkColumnExpr(x.Expr) {
			return false
		}
	case *ast.ColumnNameExpr:
		return true
	}
	return false
}

func (c *conditionChecker) checkBinaryOperation(b *ast.BinaryOperationExpr) bool {
	switch b.Op {
	case opcode.OrOr:
		return c.check(b.L) && c.check(b.R)
	case opcode.AndAnd:
		return c.check(b.L) && c.check(b.R)
	case opcode.EQ, opcode.NE, opcode.GE, opcode.GT, opcode.LE, opcode.LT:
		if b.L.IsStatic() {
			return c.checkColumnExpr(b.R)
		} else if b.R.IsStatic() {
			return c.checkColumnExpr(b.L)
		}
	}
	return false
}

func (c *conditionChecker) checkColumnExpr(expr ast.ExprNode) bool {
	cn, ok := expr.(*ast.ColumnNameExpr)
	if !ok {
		return false
	}
	if cn.Refer.Table.Name.L != c.tableName.L {
		return false
	}
	if cn.Refer.Column.Name.L != c.idx.Columns[c.columnOffset].Name.L {
		return false
	}
	return true
}
