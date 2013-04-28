// Copyright 2013 The llgo Authors.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package llgo

import (
	"code.google.com/p/go.exp/go/types"
	"github.com/axw/gollvm/llvm"
	"go/ast"
)

// hasDefer checks if a function contains any defer
// statements.
func hasDefer(f *function, body *ast.BlockStmt) bool {
	var hasdefer bool
	ast.Inspect(body, func(n ast.Node) bool {
		// Even if hasdefer is set, the inspection
		// will still continue on to sibling nodes.
		if hasdefer {
			return false
		} else {
			switch n.(type) {
			case *ast.DeferStmt:
				hasdefer = true
				return false
			case *ast.FuncLit:
				// Don't inspect function literals.
				return false
			}
			return true
		}
	})
	return hasdefer
}

// hasCallExpr checks if a function has any call expressions.
//
// This is used to avoid creating an unwind block. Later we
// might want to merge this with a pass that does escape analysis
// (looking for calls that capture variables, etc.)
func hasCallExpr(body *ast.BlockStmt) bool {
	var hascall bool
	ast.Inspect(body, func(n ast.Node) bool {
		if hascall {
			return false
		} else {
			switch n.(type) {
			case *ast.GoStmt, *ast.DeferStmt:
				return false
			case *ast.CallExpr:
				hascall = true
				return false
			case *ast.FuncLit:
				return false
			}
			return true
		}
	})
	return hascall
}

// makeDeferBlock creates a basic block for handling
// defer statements, and code is emitted to allocate and
// initialise a deferred function anchor point.
//
// This must be called before generating any code for
// the function body (not including allocating space
// for parameters and results).
func (c *compiler) makeDeferBlock(f *function, body *ast.BlockStmt) {
	currblock := c.builder.GetInsertBlock()
	defer c.builder.SetInsertPointAtEnd(currblock)

	// Create space for a pointer on the stack, which
	// we'll store the first panic structure in.
	//
	// TODO consider having stack space for one (or few)
	// defer statements, to avoid heap allocation.
	f.deferptr = c.createTypeMalloc(c.target.IntPtrType())
	f.deferblock = llvm.AddBasicBlock(currblock.Parent(), "")
	if hasCallExpr(body) {
		f.unwindblock = llvm.AddBasicBlock(currblock.Parent(), "")
		f.unwindblock.MoveAfter(currblock)
		f.deferblock.MoveAfter(f.unwindblock)
	} else {
		f.deferblock.MoveAfter(currblock)
	}

	// Create a landingpad/unwind target basic block.
	if !f.unwindblock.IsNil() {
		c.builder.SetInsertPointAtEnd(f.unwindblock)
		i8ptr := llvm.PointerType(llvm.Int8Type(), 0)
		restyp := llvm.StructType([]llvm.Type{i8ptr, llvm.Int32Type()}, false)
		pers := c.module.Module.NamedFunction("__gxx_personality_v0")
		if pers.IsNil() {
			persftyp := llvm.FunctionType(llvm.Int32Type(), nil, true)
			pers = llvm.AddFunction(c.module.Module, "__gxx_personality_v0", persftyp)
		}
		lp := c.builder.CreateLandingPad(restyp, pers, 1, "")
		lp.AddClause(llvm.ConstNull(i8ptr))
		c.builder.CreateBr(f.deferblock)
	}

	// Create a real return instruction.
	c.builder.SetInsertPointAtEnd(f.deferblock)
	ptrval := c.builder.CreateLoad(f.deferptr, "")
	rundefers := c.NamedFunction("runtime.rundefers", "func f(uintptr)")
	c.builder.CreateCall(rundefers, []llvm.Value{ptrval}, "")
	if len(f.results) == 0 {
		c.builder.CreateRetVoid()
	} else {
		values := make([]llvm.Value, len(f.results))
		for i, v := range f.results {
			values[i] = c.objectdata[v].Value.LLVMValue()
		}
		if len(values) == 1 {
			c.builder.CreateRet(values[0])
		} else {
			c.builder.CreateAggregateRet(values)
		}
	}
}

func (c *compiler) VisitDeferStmt(stmt *ast.DeferStmt) {
	// Evaluate function, store on the stack.
	fn := c.VisitExpr(stmt.Call.Fun).(*LLVMValue)
	fntype := underlyingType(fn.Type()).(*types.Signature)

	// Evaluate args.
	args := c.evalCallArgs(fntype, stmt.Call.Args)

	// Call "runtime.pushdefer" to add fn+argValues to the defer stack
	f := c.functions.top()
	pushdefer := c.NamedFunction("runtime.pushdefer", "func f(f_ func(), top *uintptr)")
	funcval := c.indirectFunction(fn, args, stmt.Call.Ellipsis.IsValid())
	c.builder.CreateCall(pushdefer, []llvm.Value{funcval.LLVMValue(), f.deferptr}, "")
}
