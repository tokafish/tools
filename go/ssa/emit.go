// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

// Helpers for emitting SSA instructions.

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/internal/typeparams"
)

// emitNew emits to f a new (heap Alloc) instruction allocating an
// object of type typ.  pos is the optional source location.
func emitNew(f *Function, typ types.Type, pos token.Pos) *Alloc {
	v := &Alloc{Heap: true}
	v.setType(types.NewPointer(typ))
	v.setPos(pos)
	f.emit(v)
	return v
}

// emitLoad emits to f an instruction to load the address addr into a
// new temporary, and returns the value so defined.
func emitLoad(f *Function, addr Value) *UnOp {
	v := &UnOp{Op: token.MUL, X: addr}
	v.setType(deref(typeparams.CoreType(addr.Type())))
	f.emit(v)
	return v
}

// emitDebugRef emits to f a DebugRef pseudo-instruction associating
// expression e with value v.
func emitDebugRef(f *Function, e ast.Expr, v Value, isAddr bool) {
	if !f.debugInfo() {
		return // debugging not enabled
	}
	if v == nil || e == nil {
		panic("nil")
	}
	var obj types.Object
	e = unparen(e)
	if id, ok := e.(*ast.Ident); ok {
		if isBlankIdent(id) {
			return
		}
		obj = f.objectOf(id)
		switch obj.(type) {
		case *types.Nil, *types.Const, *types.Builtin:
			return
		}
	}
	f.emit(&DebugRef{
		X:      v,
		Expr:   e,
		IsAddr: isAddr,
		object: obj,
	})
}

// emitArith emits to f code to compute the binary operation op(x, y)
// where op is an eager shift, logical or arithmetic operation.
// (Use emitCompare() for comparisons and Builder.logicalBinop() for
// non-eager operations.)
func emitArith(f *Function, op token.Token, x, y Value, t types.Type, pos token.Pos) Value {
	switch op {
	case token.SHL, token.SHR:
		x = emitConv(f, x, t)
		// y may be signed or an 'untyped' constant.

		// There is a runtime panic if y is signed and <0. Instead of inserting a check for y<0
		// and converting to an unsigned value (like the compiler) leave y as is.

		if isUntyped(y.Type().Underlying()) {
			// Untyped conversion:
			// Spec https://go.dev/ref/spec#Operators:
			// The right operand in a shift expression must have integer type or be an untyped constant
			// representable by a value of type uint.
			y = emitConv(f, y, types.Typ[types.Uint])
		}

	case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR, token.AND_NOT:
		x = emitConv(f, x, t)
		y = emitConv(f, y, t)

	default:
		panic("illegal op in emitArith: " + op.String())

	}
	v := &BinOp{
		Op: op,
		X:  x,
		Y:  y,
	}
	v.setPos(pos)
	v.setType(t)
	return f.emit(v)
}

// emitCompare emits to f code compute the boolean result of
// comparison comparison 'x op y'.
func emitCompare(f *Function, op token.Token, x, y Value, pos token.Pos) Value {
	xt := x.Type().Underlying()
	yt := y.Type().Underlying()

	// Special case to optimise a tagless SwitchStmt so that
	// these are equivalent
	//   switch { case e: ...}
	//   switch true { case e: ... }
	//   if e==true { ... }
	// even in the case when e's type is an interface.
	// TODO(adonovan): opt: generalise to x==true, false!=y, etc.
	if x == vTrue && op == token.EQL {
		if yt, ok := yt.(*types.Basic); ok && yt.Info()&types.IsBoolean != 0 {
			return y
		}
	}

	if types.Identical(xt, yt) {
		// no conversion necessary
	} else if isNonTypeParamInterface(x.Type()) {
		y = emitConv(f, y, x.Type())
	} else if isNonTypeParamInterface(y.Type()) {
		x = emitConv(f, x, y.Type())
	} else if _, ok := x.(*Const); ok {
		x = emitConv(f, x, y.Type())
	} else if _, ok := y.(*Const); ok {
		y = emitConv(f, y, x.Type())
	} else {
		// other cases, e.g. channels.  No-op.
	}

	v := &BinOp{
		Op: op,
		X:  x,
		Y:  y,
	}
	v.setPos(pos)
	v.setType(tBool)
	return f.emit(v)
}

// isValuePreserving returns true if a conversion from ut_src to
// ut_dst is value-preserving, i.e. just a change of type.
// Precondition: neither argument is a named type.
func isValuePreserving(ut_src, ut_dst types.Type) bool {
	// Identical underlying types?
	if structTypesIdentical(ut_dst, ut_src) {
		return true
	}

	switch ut_dst.(type) {
	case *types.Chan:
		// Conversion between channel types?
		_, ok := ut_src.(*types.Chan)
		return ok

	case *types.Pointer:
		// Conversion between pointers with identical base types?
		_, ok := ut_src.(*types.Pointer)
		return ok
	}
	return false
}

// isSliceToArrayPointer reports whether ut_src is a slice type
// that can be converted to a pointer to an array type ut_dst.
// Precondition: neither argument is a named type.
func isSliceToArrayPointer(ut_src, ut_dst types.Type) bool {
	if slice, ok := ut_src.(*types.Slice); ok {
		if ptr, ok := ut_dst.(*types.Pointer); ok {
			if arr, ok := ptr.Elem().Underlying().(*types.Array); ok {
				return types.Identical(slice.Elem(), arr.Elem())
			}
		}
	}
	return false
}

// isSliceToArray reports whether ut_src is a slice type
// that can be converted to an array type ut_dst.
// Precondition: neither argument is a named type.
func isSliceToArray(ut_src, ut_dst types.Type) bool {
	if slice, ok := ut_src.(*types.Slice); ok {
		if arr, ok := ut_dst.(*types.Array); ok {
			return types.Identical(slice.Elem(), arr.Elem())
		}
	}
	return false
}

// emitConv emits to f code to convert Value val to exactly type typ,
// and returns the converted value.  Implicit conversions are required
// by language assignability rules in assignments, parameter passing,
// etc.
func emitConv(f *Function, val Value, typ types.Type) Value {
	t_src := val.Type()

	// Identical types?  Conversion is a no-op.
	if types.Identical(t_src, typ) {
		return val
	}
	ut_dst := typ.Underlying()
	ut_src := t_src.Underlying()

	dst_types := typeSetOf(ut_dst)
	src_types := typeSetOf(ut_src)

	// Just a change of type, but not value or representation?
	preserving := underIs(src_types, func(s types.Type) bool {
		return underIs(dst_types, func(d types.Type) bool {
			return s != nil && d != nil && isValuePreserving(s, d) // all (s -> d) are value preserving.
		})
	})
	if preserving {
		c := &ChangeType{X: val}
		c.setType(typ)
		return f.emit(c)
	}

	// Conversion to, or construction of a value of, an interface type?
	if isNonTypeParamInterface(typ) {
		// Assignment from one interface type to another?
		if isNonTypeParamInterface(t_src) {
			c := &ChangeInterface{X: val}
			c.setType(typ)
			return f.emit(c)
		}

		// Untyped nil constant?  Return interface-typed nil constant.
		if ut_src == tUntypedNil {
			return zeroConst(typ)
		}

		// Convert (non-nil) "untyped" literals to their default type.
		if t, ok := ut_src.(*types.Basic); ok && t.Info()&types.IsUntyped != 0 {
			val = emitConv(f, val, types.Default(ut_src))
		}

		mi := &MakeInterface{X: val}
		mi.setType(typ)
		return f.emit(mi)
	}

	// Conversion of a compile-time constant value?
	if c, ok := val.(*Const); ok {
		if isBasic(ut_dst) || c.Value == nil {
			// Conversion of a compile-time constant to
			// another constant type results in a new
			// constant of the destination type and
			// (initially) the same abstract value.
			// We don't truncate the value yet.
			return NewConst(c.Value, typ)
		}

		// We're converting from constant to non-constant type,
		// e.g. string -> []byte/[]rune.
	}

	// Conversion from slice to array pointer?
	slice2ptr := underIs(src_types, func(s types.Type) bool {
		return underIs(dst_types, func(d types.Type) bool {
			return s != nil && d != nil && isSliceToArrayPointer(s, d) // all (s->d) are slice to array pointer conversion.
		})
	})
	if slice2ptr {
		c := &SliceToArrayPointer{X: val}
		c.setType(typ)
		return f.emit(c)
	}

	// Conversion from slice to array?
	slice2array := underIs(src_types, func(s types.Type) bool {
		return underIs(dst_types, func(d types.Type) bool {
			return s != nil && d != nil && isSliceToArray(s, d) // all (s->d) are slice to array conversion.
		})
	})
	if slice2array {
		return emitSliceToArray(f, val, typ)
	}

	// A representation-changing conversion?
	// All of ut_src or ut_dst is basic, byte slice, or rune slice?
	if isBasicConvTypes(src_types) || isBasicConvTypes(dst_types) {
		c := &Convert{X: val}
		c.setType(typ)
		return f.emit(c)
	}

	panic(fmt.Sprintf("in %s: cannot convert %s (%s) to %s", f, val, val.Type(), typ))
}

// emitTypeCoercion emits to f code to coerce the type of a
// Value v to exactly type typ, and returns the coerced value.
//
// Requires that coercing v.Typ() to typ is a value preserving change.
//
// Currently used only when v.Type() is a type instance of typ or vice versa.
// A type v is a type instance of a type t if there exists a
// type parameter substitution σ s.t. σ(v) == t. Example:
//
//	σ(func(T) T) == func(int) int for σ == [T ↦ int]
//
// This happens in instantiation wrappers for conversion
// from an instantiation to a parameterized type (and vice versa)
// with σ substituting f.typeparams by f.typeargs.
func emitTypeCoercion(f *Function, v Value, typ types.Type) Value {
	if types.Identical(v.Type(), typ) {
		return v // no coercion needed
	}
	// TODO(taking): for instances should we record which side is the instance?
	c := &ChangeType{
		X: v,
	}
	c.setType(typ)
	f.emit(c)
	return c
}

// emitStore emits to f an instruction to store value val at location
// addr, applying implicit conversions as required by assignability rules.
func emitStore(f *Function, addr, val Value, pos token.Pos) *Store {
	s := &Store{
		Addr: addr,
		Val:  emitConv(f, val, deref(addr.Type())),
		pos:  pos,
	}
	f.emit(s)
	return s
}

// emitJump emits to f a jump to target, and updates the control-flow graph.
// Postcondition: f.currentBlock is nil.
func emitJump(f *Function, target *BasicBlock) {
	b := f.currentBlock
	b.emit(new(Jump))
	addEdge(b, target)
	f.currentBlock = nil
}

// emitIf emits to f a conditional jump to tblock or fblock based on
// cond, and updates the control-flow graph.
// Postcondition: f.currentBlock is nil.
func emitIf(f *Function, cond Value, tblock, fblock *BasicBlock) {
	b := f.currentBlock
	b.emit(&If{Cond: cond})
	addEdge(b, tblock)
	addEdge(b, fblock)
	f.currentBlock = nil
}

// emitExtract emits to f an instruction to extract the index'th
// component of tuple.  It returns the extracted value.
func emitExtract(f *Function, tuple Value, index int) Value {
	e := &Extract{Tuple: tuple, Index: index}
	e.setType(tuple.Type().(*types.Tuple).At(index).Type())
	return f.emit(e)
}

// emitTypeAssert emits to f a type assertion value := x.(t) and
// returns the value.  x.Type() must be an interface.
func emitTypeAssert(f *Function, x Value, t types.Type, pos token.Pos) Value {
	a := &TypeAssert{X: x, AssertedType: t}
	a.setPos(pos)
	a.setType(t)
	return f.emit(a)
}

// emitTypeTest emits to f a type test value,ok := x.(t) and returns
// a (value, ok) tuple.  x.Type() must be an interface.
func emitTypeTest(f *Function, x Value, t types.Type, pos token.Pos) Value {
	a := &TypeAssert{
		X:            x,
		AssertedType: t,
		CommaOk:      true,
	}
	a.setPos(pos)
	a.setType(types.NewTuple(
		newVar("value", t),
		varOk,
	))
	return f.emit(a)
}

// emitTailCall emits to f a function call in tail position.  The
// caller is responsible for all fields of 'call' except its type.
// Intended for wrapper methods.
// Precondition: f does/will not use deferred procedure calls.
// Postcondition: f.currentBlock is nil.
func emitTailCall(f *Function, call *Call) {
	tresults := f.Signature.Results()
	nr := tresults.Len()
	if nr == 1 {
		call.typ = tresults.At(0).Type()
	} else {
		call.typ = tresults
	}
	tuple := f.emit(call)
	var ret Return
	switch nr {
	case 0:
		// no-op
	case 1:
		ret.Results = []Value{tuple}
	default:
		for i := 0; i < nr; i++ {
			v := emitExtract(f, tuple, i)
			// TODO(adonovan): in principle, this is required:
			//   v = emitConv(f, o.Type, f.Signature.Results[i].Type)
			// but in practice emitTailCall is only used when
			// the types exactly match.
			ret.Results = append(ret.Results, v)
		}
	}
	f.emit(&ret)
	f.currentBlock = nil
}

// emitImplicitSelections emits to f code to apply the sequence of
// implicit field selections specified by indices to base value v, and
// returns the selected value.
//
// If v is the address of a struct, the result will be the address of
// a field; if it is the value of a struct, the result will be the
// value of a field.
func emitImplicitSelections(f *Function, v Value, indices []int, pos token.Pos) Value {
	for _, index := range indices {
		fld := typeparams.CoreType(deref(v.Type())).(*types.Struct).Field(index)

		if isPointer(v.Type()) {
			instr := &FieldAddr{
				X:     v,
				Field: index,
			}
			instr.setPos(pos)
			instr.setType(types.NewPointer(fld.Type()))
			v = f.emit(instr)
			// Load the field's value iff indirectly embedded.
			if isPointer(fld.Type()) {
				v = emitLoad(f, v)
			}
		} else {
			instr := &Field{
				X:     v,
				Field: index,
			}
			instr.setPos(pos)
			instr.setType(fld.Type())
			v = f.emit(instr)
		}
	}
	return v
}

// emitFieldSelection emits to f code to select the index'th field of v.
//
// If wantAddr, the input must be a pointer-to-struct and the result
// will be the field's address; otherwise the result will be the
// field's value.
// Ident id is used for position and debug info.
func emitFieldSelection(f *Function, v Value, index int, wantAddr bool, id *ast.Ident) Value {
	fld := typeparams.CoreType(deref(v.Type())).(*types.Struct).Field(index)
	if isPointer(v.Type()) {
		instr := &FieldAddr{
			X:     v,
			Field: index,
		}
		instr.setPos(id.Pos())
		instr.setType(types.NewPointer(fld.Type()))
		v = f.emit(instr)
		// Load the field's value iff we don't want its address.
		if !wantAddr {
			v = emitLoad(f, v)
		}
	} else {
		instr := &Field{
			X:     v,
			Field: index,
		}
		instr.setPos(id.Pos())
		instr.setType(fld.Type())
		v = f.emit(instr)
	}
	emitDebugRef(f, id, v, wantAddr)
	return v
}

// emitSliceToArray emits to f code to convert a slice value to an array value.
//
// Precondition: all types in type set of typ are arrays and convertible to all
// types in the type set of val.Type().
func emitSliceToArray(f *Function, val Value, typ types.Type) Value {
	// Emit the following:
	// if val == nil && len(typ) == 0 {
	//    ptr = &[0]T{}
	// } else {
	//	  ptr = SliceToArrayPointer(val)
	// }
	// v = *ptr

	ptype := types.NewPointer(typ)
	p := &SliceToArrayPointer{X: val}
	p.setType(ptype)
	ptr := f.emit(p)

	nilb := f.newBasicBlock("slicetoarray.nil")
	nonnilb := f.newBasicBlock("slicetoarray.nonnil")
	done := f.newBasicBlock("slicetoarray.done")

	cond := emitCompare(f, token.EQL, ptr, zeroConst(ptype), token.NoPos)
	emitIf(f, cond, nilb, nonnilb)
	f.currentBlock = nilb

	zero := f.addLocal(typ, token.NoPos)
	emitJump(f, done)
	f.currentBlock = nonnilb

	emitJump(f, done)
	f.currentBlock = done

	phi := &Phi{Edges: []Value{zero, ptr}, Comment: "slicetoarray"}
	phi.pos = val.Pos()
	phi.setType(ptype)
	x := f.emit(phi)
	unOp := &UnOp{Op: token.MUL, X: x}
	unOp.setType(typ)
	return f.emit(unOp)
}

// zeroValue emits to f code to produce a zero value of type t,
// and returns it.
func zeroValue(f *Function, t types.Type) Value {
	switch t.Underlying().(type) {
	case *types.Struct, *types.Array:
		return emitLoad(f, f.addLocal(t, token.NoPos))
	default:
		return zeroConst(t)
	}
}

// createRecoverBlock emits to f a block of code to return after a
// recovered panic, and sets f.Recover to it.
//
// If f's result parameters are named, the code loads and returns
// their current values, otherwise it returns the zero values of their
// type.
//
// Idempotent.
func createRecoverBlock(f *Function) {
	if f.Recover != nil {
		return // already created
	}
	saved := f.currentBlock

	f.Recover = f.newBasicBlock("recover")
	f.currentBlock = f.Recover

	var results []Value
	if f.namedResults != nil {
		// Reload NRPs to form value tuple.
		for _, r := range f.namedResults {
			results = append(results, emitLoad(f, r))
		}
	} else {
		R := f.Signature.Results()
		for i, n := 0, R.Len(); i < n; i++ {
			T := R.At(i).Type()

			// Return zero value of each result type.
			results = append(results, zeroValue(f, T))
		}
	}
	f.emit(&Return{Results: results})

	f.currentBlock = saved
}
