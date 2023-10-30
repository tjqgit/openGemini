/*
Copyright 2023 Huawei Cloud Computing Technologies Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sparseindex

import (
	"github.com/openGemini/openGemini/engine/hybridqp"
	"github.com/openGemini/openGemini/lib/binaryfilterfunc"
	"github.com/openGemini/openGemini/lib/errno"
	"github.com/openGemini/openGemini/lib/record"
	"github.com/openGemini/openGemini/lib/rpn"
	"github.com/openGemini/openGemini/open_src/influx/influxql"
	"github.com/openGemini/openGemini/open_src/vm/protoparser/influx"
)

type checkInRangeFunc func(rgs []*Range) (Mark, error)

type KeyCondition interface {
	HavePrimaryKey() bool
	GetMaxKeyIndex() int
	IsFirstPrimaryKey() bool
	CanDoBinarySearch() bool
	MayBeInRange(usedKeySize int, indexLeft []*FieldRef, indexRight []*FieldRef, dataTypes []int) (bool, error)
}

type KeyConditionImpl struct {
	pkSchema record.Schemas
	rpn      []*RPNElement
}

func NewKeyCondition(timeCondition, condition influxql.Expr, pkSchema record.Schemas) (*KeyConditionImpl, error) {
	kc := &KeyConditionImpl{pkSchema: pkSchema}
	cols := genIndexColumnsBySchema(pkSchema)
	// use "AND" to connect the time condition to other conditions.
	combineCondition := binaryfilterfunc.CombineConditionWithAnd(timeCondition, condition)
	rpnExpr := rpn.ConvertToRPNExpr(combineCondition)
	if err := kc.convertToRPNElem(rpnExpr, cols); err != nil {
		return nil, err
	}
	return kc, nil
}

func (kc *KeyConditionImpl) convertToRPNElem(
	rpnExpr *rpn.RPNExpr,
	cols []*ColumnRef,
) error {
	var fieldCount int
	for i, expr := range rpnExpr.Val {
		switch v := expr.(type) {
		case influxql.Token:
			switch v {
			case influxql.AND:
				if fieldCount == 0 {
					kc.rpn = append(kc.rpn, &RPNElement{op: rpn.AND})
				} else {
					fieldCount--
				}
			case influxql.OR:
				if fieldCount == 0 {
					kc.rpn = append(kc.rpn, &RPNElement{op: rpn.OR})
				} else {
					fieldCount--
				}
			case influxql.EQ, influxql.LT, influxql.LTE, influxql.GT, influxql.GTE, influxql.NEQ:
			default:
				return errno.NewError(errno.ErrRPNOp, v)
			}
		case *influxql.VarRef:
			// a filter expression is converted into three elements. such as, (A = 'a') => (A 'a' =).
			// the first element is a field. rpnExpr.Val[i]
			// the second element is a value. rpnExpr.Val[i+1]
			// the third element is a operator. rpnExpr.Val[i+2]
			idx := kc.pkSchema.FieldIndex(v.Val)
			if idx < 0 {
				fieldCount++
				continue
			}
			if i+2 >= len(rpnExpr.Val) {
				return errno.NewError(errno.ErrRPNElemNum)
			}
			value := rpnExpr.Val[i+1]
			op, ok := rpnExpr.Val[i+2].(influxql.Token)
			if !ok {
				return errno.NewError(errno.ErrRPNElemOp)
			}
			if err := kc.genRPNElementByVal(value, op, cols, idx); err != nil {
				return err
			}
		case *influxql.StringLiteral, *influxql.NumberLiteral, *influxql.IntegerLiteral, *influxql.BooleanLiteral:
		default:
			return errno.NewError(errno.ErrRPNExpr, v)
		}
	}
	return nil
}

func (kc *KeyConditionImpl) genRPNElementByVal(
	rhs interface{},
	op influxql.Token,
	cols []*ColumnRef,
	idx int,
) error {
	rpnElem := &RPNElement{keyColumn: idx}
	value := NewFieldRef(cols, idx, 0)
	switch rhs := rhs.(type) {
	case *influxql.StringLiteral:
		value.cols[idx].column.AppendString(rhs.Val)
	case *influxql.NumberLiteral:
		value.cols[idx].column.AppendFloat(rhs.Val)
	case *influxql.IntegerLiteral:
		value.cols[idx].column.AppendInteger(rhs.Val)
	case *influxql.BooleanLiteral:
		value.cols[idx].column.AppendBoolean(rhs.Val)
	default:
		return errno.NewError(errno.ErrRPNElement, rhs)
	}
	if value.cols[idx].column.Len > 1 {
		value.row = value.cols[idx].column.Len - 1
	}
	if ok := genRPNElementByOp(op, value, rpnElem); ok {
		kc.rpn = append(kc.rpn, rpnElem)
	}
	return nil
}

// applyChainToRange apply the monotonicity of each function on a specific range.
func (kc *KeyConditionImpl) applyChainToRange(
	_ *Range,
	_ []*FunctionBase,
	_ int,
	_ bool,
) *Range {
	return &Range{}
}

// CheckInRange check Whether the condition and its negation are feasible
// in the direct product of single column ranges specified by hyper-rectangle.
func (kc *KeyConditionImpl) CheckInRange(
	rgs []*Range,
	dataTypes []int,
) (Mark, error) {
	var err error
	var singlePoint bool
	var rpnStack []Mark
	for _, elem := range kc.rpn {
		if elem.op == rpn.InRange || elem.op == rpn.NotInRange {
			rpnStack = kc.checkInRangeForRange(rgs, rpnStack, elem, dataTypes, singlePoint)
		} else if elem.op == rpn.InSet || elem.op == rpn.NotInSet {
			rpnStack, err = kc.checkInRangeForSet(rgs, rpnStack, elem, dataTypes, singlePoint)
			if err != nil {
				return Mark{}, err
			}
		} else if elem.op == rpn.AND {
			rpnStack, err = kc.checkInRangeForAnd(rpnStack)
			if err != nil {
				return Mark{}, err
			}
		} else if elem.op == rpn.OR {
			rpnStack, err = kc.checkInRangeForOr(rpnStack)
			if err != nil {
				return Mark{}, err
			}
		} else {
			return Mark{}, errno.NewError(errno.ErrUnknownOpInCondition)
		}
	}
	if len(rpnStack) != 1 {
		return Mark{}, errno.NewError(errno.ErrInvalidStackInCondition)
	}
	return rpnStack[0], nil
}

func (kc *KeyConditionImpl) checkInRangeForRange(
	rgs []*Range,
	rpnStack []Mark,
	elem *RPNElement,
	dataTypes []int,
	singlePoint bool,
) []Mark {
	keyRange := rgs[elem.keyColumn]
	if len(elem.monotonicChains) > 0 {
		newRange := kc.applyChainToRange(keyRange, elem.monotonicChains, dataTypes[elem.keyColumn], singlePoint)
		if newRange != nil {
			rpnStack = append(rpnStack, NewMark(true, true))
			return rpnStack
		}
		keyRange = newRange
	}
	intersects := elem.rg.intersectsRange(keyRange)
	contains := elem.rg.containsRange(keyRange)
	rpnStack = append(rpnStack, NewMark(intersects, !contains))
	if elem.op == rpn.NotInRange {
		rpnStack[len(rpnStack)-1] = rpnStack[len(rpnStack)-1].Not()
	}
	return rpnStack
}

func (kc *KeyConditionImpl) checkInRangeForSet(
	rgs []*Range,
	rpnStack []Mark,
	elem *RPNElement,
	dataTypes []int,
	singlePoint bool,
) ([]Mark, error) {
	if elem.setIndex.Not() {
		return nil, errno.NewError(errno.ErrRPNSetInNotCreated)
	}
	rpnStack = append(rpnStack, elem.setIndex.checkInRange(rgs, dataTypes, singlePoint))
	if elem.op == rpn.NotInSet {
		rpnStack[len(rpnStack)-1] = rpnStack[len(rpnStack)-1].Not()
	}
	return rpnStack, nil
}

func (kc *KeyConditionImpl) checkInRangeForAnd(
	rpnStack []Mark,
) ([]Mark, error) {
	if len(rpnStack) == 0 {
		return nil, errno.NewError(errno.ErrRPNIsNullForAnd)
	}
	rg1 := (rpnStack)[len(rpnStack)-1]
	rpnStack = rpnStack[:len(rpnStack)-1]
	rpnStack[len(rpnStack)-1] = rpnStack[len(rpnStack)-1].And(rg1)
	return rpnStack, nil
}

func (kc *KeyConditionImpl) checkInRangeForOr(
	rpnStack []Mark,
) ([]Mark, error) {
	if len(rpnStack) == 0 {
		return nil, errno.NewError(errno.ErrRPNIsNullForOR)
	}
	rg1 := rpnStack[len(rpnStack)-1]
	rpnStack = rpnStack[:len(rpnStack)-1]
	rpnStack[len(rpnStack)-1] = rpnStack[len(rpnStack)-1].Or(rg1)
	return rpnStack, nil
}

// checkInAnyRange check Whether the condition and its negation are feasible
// in the direct product of single column ranges specified by any hyper-rectangle.
func (kc *KeyConditionImpl) checkInAnyRange(
	keySize int,
	leftKeys []*FieldRef,
	rightKeys []*FieldRef,
	leftBounded bool,
	rightBounded bool,
	rgs []*Range,
	dataTypes []int,
	prefixSize int,
	initMask Mark,
	callBack checkInRangeFunc,
) (Mark, error) {
	if !leftBounded && !rightBounded {
		return callBack(rgs)
	}
	if leftBounded && rightBounded {
		for prefixSize < keySize {
			if leftKeys[prefixSize].Equals(rightKeys[prefixSize]) {
				rgs[prefixSize] = NewRange(leftKeys[prefixSize], leftKeys[prefixSize], true, true)
				prefixSize++
			} else {
				break
			}
		}
	}
	if prefixSize == keySize {
		return callBack(rgs)
	}

	// (x1 .. x2) x (-inf .. +inf)
	res, completed, err := kc.checkRangeLeftRightBound(leftKeys, rightKeys, leftBounded, rightBounded, keySize, initMask, rgs, dataTypes, prefixSize, callBack)
	if err != nil || completed {
		return res, err
	}

	// [x1] x [y1 .. +inf)
	if leftBounded {
		res, completed, err = kc.checkRangeLeftBound(keySize, leftKeys, rightKeys, rgs, dataTypes, prefixSize, initMask, res, callBack)
		if err != nil || completed {
			return res, err
		}
	}

	// [x2] x (-inf .. y2]
	if rightBounded {
		res, completed, err = kc.checkRangeRightBound(keySize, leftKeys, rightKeys, rgs, dataTypes, prefixSize, initMask, res, callBack)
		if err != nil || completed {
			return res, err
		}
	}
	return res, nil
}

func (kc *KeyConditionImpl) checkRangeLeftRightBound(
	leftKeys []*FieldRef,
	rightKeys []*FieldRef,
	leftBounded bool,
	rightBounded bool,
	keySize int,
	initMask Mark,
	rgs []*Range,
	dataTypes []int,
	prefixSize int,
	callBack checkInRangeFunc,
) (Mark, bool, error) {
	if prefixSize+1 == keySize {
		if leftBounded && rightBounded {
			rgs[prefixSize] = NewRange(leftKeys[prefixSize], rightKeys[prefixSize], true, true)
		} else if leftBounded {
			rgs[prefixSize] = createLeftBounded(leftKeys[prefixSize], true, false)
		} else if rightBounded {
			rgs[prefixSize] = createRightBounded(rightKeys[prefixSize], true, false)
		}
		m, err := callBack(rgs)
		return m, true, err
	}
	if leftBounded && rightBounded {
		rgs[prefixSize] = NewRange(leftKeys[prefixSize], rightKeys[prefixSize], false, false)
	} else if leftBounded {
		rgs[prefixSize] = createLeftBounded(leftKeys[prefixSize], false, dataTypes[prefixSize] == influx.Field_Type_Unknown)
	} else if rightBounded {
		rgs[prefixSize] = createRightBounded(rightKeys[prefixSize], false, dataTypes[prefixSize] == influx.Field_Type_Unknown)
	}
	for i := prefixSize + 1; i < keySize; i++ {
		if dataTypes[i] == influx.Field_Type_Unknown {
			rgs[i] = createWholeRangeIncludeBound()
		} else {
			rgs[i] = createWholeRangeWithoutBound()
		}
	}

	res := initMask
	m, err := callBack(rgs)
	if err != nil {
		return Mark{}, false, err
	}
	res = res.Or(m)

	/// There are several early-exit conditions (like the one below) hereinafter.
	if res.isComplete() {
		return res, true, nil
	}
	return res, false, nil
}

func (kc *KeyConditionImpl) checkRangeLeftBound(
	keySize int,
	leftKeys []*FieldRef,
	rightKeys []*FieldRef,
	rgs []*Range,
	dataTypes []int,
	prefixSize int,
	initMask Mark,
	res Mark,
	callBack checkInRangeFunc,
) (Mark, bool, error) {
	rgs[prefixSize] = NewRange(leftKeys[prefixSize], leftKeys[prefixSize], true, true)
	mark, err := kc.checkInAnyRange(keySize, leftKeys, rightKeys, true, false, rgs, dataTypes, prefixSize+1, initMask, callBack)
	if err != nil {
		return res, false, err
	}
	res = res.Or(mark)
	if res.isComplete() {
		return res, true, nil
	}
	return res, false, nil
}

func (kc *KeyConditionImpl) checkRangeRightBound(
	keySize int,
	leftKeys []*FieldRef,
	rightKeys []*FieldRef,
	rgs []*Range,
	dataTypes []int,
	prefixSize int,
	initMask Mark,
	res Mark,
	callBack checkInRangeFunc,
) (Mark, bool, error) {
	rgs[prefixSize] = NewRange(rightKeys[prefixSize], rightKeys[prefixSize], true, true)
	mark, err := kc.checkInAnyRange(keySize, leftKeys, rightKeys, false, true, rgs, dataTypes, prefixSize+1, initMask, callBack)
	if err != nil {
		return mark, false, err
	}
	res = res.Or(mark)
	if res.isComplete() {
		return mark, true, nil
	}
	return mark, false, nil
}

// MayBeInRange is used to check whether the condition is likely to be in the target range.
func (kc *KeyConditionImpl) MayBeInRange(
	usedKeySize int,
	leftKeys []*FieldRef,
	rightKeys []*FieldRef,
	dataTypes []int,
) (bool, error) {
	initMask := ConsiderOnlyBeTrue
	keyRgs := make([]*Range, 0, usedKeySize)
	for i := 0; i < usedKeySize; i++ {
		if dataTypes[i] == influx.Field_Type_Unknown {
			keyRgs = append(keyRgs, createWholeRangeIncludeBound())
		} else {
			keyRgs = append(keyRgs, createWholeRangeWithoutBound())
		}
	}
	mark, err := kc.checkInAnyRange(
		usedKeySize, leftKeys, rightKeys, true, true, keyRgs, dataTypes, 0, initMask,
		func(rgs []*Range) (Mark, error) {
			res, err := kc.CheckInRange(rgs, dataTypes)
			return res, err
		})
	if err != nil {
		return false, err
	}
	return mark.canBeTrue, nil
}

func (kc *KeyConditionImpl) HavePrimaryKey() bool {
	return len(kc.rpn) > 0
}

func (kc *KeyConditionImpl) GetMaxKeyIndex() int {
	res := -1
	for i := range kc.rpn {
		if kc.rpn[i].op <= rpn.NotInSet {
			res = hybridqp.MaxInt(res, kc.rpn[i].keyColumn)
		}
	}
	return res
}

func (kc *KeyConditionImpl) IsFirstPrimaryKey() bool {
	return kc.GetMaxKeyIndex() == 0
}

func (kc *KeyConditionImpl) CanDoBinarySearch() bool {
	return kc.IsFirstPrimaryKey()
}

func (kc *KeyConditionImpl) GetRPN() []*RPNElement {
	return kc.rpn
}

func (kc *KeyConditionImpl) SetRPN(rpn []*RPNElement) {
	kc.rpn = rpn
}
