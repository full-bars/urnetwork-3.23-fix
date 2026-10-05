package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// runH3 starts from the transport's own goroutine and wraps the host UDP socket
// through the identity's bandwidth record, so that record has to be registered
// before the transport is constructed. Registered afterwards, the socket stays
// unwrapped and every byte H3 carries goes uncounted for the life of the
// session. Nothing else in the unit tests can catch this: the wiring is the
// order of two calls in provide().
func TestBandwidthIsRegisteredBeforeTheTransportStarts(t *testing.T) {
	text, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", text, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	registerAt, transportAt := -1, -1
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch h3WiringCallee(call.Fun) {
		case "connect.RegisterProxyBandwidth":
			// Record EVERY registration, not just the first: main.go registers
			// one per proxy and one for the native identity, and the LAST of
			// them is the one that has to precede the transport.
			registerAt = fset.Position(call.Pos()).Line
		case "connect.NewPlatformTransport":
			if transportAt == -1 {
				transportAt = fset.Position(call.Pos()).Line
			}
		}
		return true
	})
	if registerAt == -1 || transportAt == -1 {
		t.Fatalf("main.go no longer calls both: RegisterProxyBandwidth at %d, NewPlatformTransport at %d", registerAt, transportAt)
	}
	if registerAt > transportAt {
		t.Fatalf("the last RegisterProxyBandwidth is called at line %d, after NewPlatformTransport at line %d: an H3 socket started by that transport would never be wrapped", registerAt, transportAt)
	}
}

// h3WiringCallee renders a call's function as "pkg.Func" or "Func", which is
// enough to recognise the two calls this test cares about.
func h3WiringCallee(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
		return f.Sel.Name
	}
	return ""
}
