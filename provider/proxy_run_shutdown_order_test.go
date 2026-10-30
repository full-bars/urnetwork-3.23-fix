package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The real provide call site must wait for cancellation before quiescing.
// The behavioral reloader tests exercise the stop and slot-wait paths.
func TestProvideQuiescesAfterCancellationBeforeWaiting(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "provide" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("provide function not found")
	}
	for i, stmt := range body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "quiesceForRun" {
			continue
		}
		if i == 0 || i+1 >= len(body.List) {
			t.Fatal("quiesce lacks shutdown neighbors")
		}
		previous, ok := body.List[i-1].(*ast.ExprStmt)
		if !ok {
			t.Fatal("quiesce must follow <-ctx.Done()")
		}
		recv, ok := previous.X.(*ast.UnaryExpr)
		if !ok || recv.Op != token.ARROW {
			t.Fatal("quiesce must follow <-ctx.Done()")
		}
		done, ok := recv.X.(*ast.CallExpr)
		if !ok || len(done.Args) != 0 {
			t.Fatal("quiesce must follow <-ctx.Done()")
		}
		sel, ok := done.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Done" {
			t.Fatal("quiesce must follow <-ctx.Done()")
		}
		ctx, ok := sel.X.(*ast.Ident)
		if !ok || ctx.Name != "ctx" {
			t.Fatal("quiesce must follow <-ctx.Done()")
		}
		next, ok := body.List[i+1].(*ast.ExprStmt)
		if !ok {
			t.Fatal("wg.Wait() must follow quiesce")
		}
		wait, ok := next.X.(*ast.CallExpr)
		if !ok || len(wait.Args) != 0 {
			t.Fatal("wg.Wait() must follow quiesce")
		}
		waitSel, ok := wait.Fun.(*ast.SelectorExpr)
		if !ok || waitSel.Sel.Name != "Wait" {
			t.Fatal("wg.Wait() must follow quiesce")
		}
		wg, ok := waitSel.X.(*ast.Ident)
		if !ok || wg.Name != "wg" {
			t.Fatal("wg.Wait() must follow quiesce")
		}
		return
	}
	t.Fatal("quiesceForRun call not found in provide")
}
