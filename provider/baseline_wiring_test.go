package main

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The last feature shipped with its startup wiring missing while every unit
// test stayed green: the functions existed, were fully tested, and nothing in
// production ever called them. So the recorder's start is proven here by reading
// the source, the same technique sn's startup_wiring_test.go uses.
//
// Three things must hold, and each is a separate mutation below:
//   - provide() calls baselineStart
//   - it passes the PROVIDER's ctx, not a fresh context.Background(), or the
//     sampler would outlive the process and become a second writer after a
//     hot swap promoted the candidate
//   - baselineStart launches the sampler, and the sampler stops on cancel
func TestProvideStartsTheBaselineRecorder(t *testing.T) {
	body := provideFuncBody(t)
	line := body.firstIndex("baselineStart")
	if line < 0 {
		t.Fatal("provide() never calls baselineStart: the recorder would be fully " +
			"implemented, fully tested, and never run, so every box would have no " +
			"baseline and nothing would fail")
	}
	// It must not be buried in a helper nothing calls.
	if !body.enclosingFuncIs("provide") {
		t.Errorf("baselineStart is called at line %d but not inside provide()'s own body; "+
			"a call in an uncalled helper does nothing", line)
	}
}

func TestBaselineStartReceivesTheProviderContext(t *testing.T) {
	// A background context here would leak the sampler goroutine past shutdown.
	// On a hot swap the parent keeps running while the candidate starts, so a
	// leaked recorder means two processes appending to the same file.
	body := provideFuncBody(t)
	call := body.callWithFirstArg("baselineStart")
	if call == nil {
		t.Fatal("provide() has no baselineStart call to inspect")
	}
	args := call.args
	if len(args) == 0 {
		t.Fatal("baselineStart is called with no arguments")
	}
	first := args[0].text
	if first != "ctx" {
		t.Errorf("baselineStart is passed %q as its context, want the provider's ctx. "+
			"A context.Background() here outlives the process and leaves a second "+
			"writer on %s after a hot swap", first, baselineFileName)
	}
	for _, a := range args {
		if strings.Contains(a.text, "context.Background()") {
			t.Errorf("baselineStart argument %q creates a fresh context; the sampler "+
				"must be tied to the provider's", a.text)
		}
	}
}

// baselineStart writes the start mark and launches the goroutine; both must
// happen, and the mark must be written BEFORE the goroutine starts so the very
// first sample has a start to be read against.
func TestBaselineStartWritesTheMarkBeforeStartingTheGoroutine(t *testing.T) {
	markLine := sourceLine(t, "baseline.go", "baselineRecord(baselineStartSample(")
	goLine := sourceLine(t, "baseline.go", "go baselineRun(ctx, done)")
	if markLine < 0 {
		t.Error("baselineStart does not write a start mark; without one a compare has no " +
			"boundary to split on and cannot find the upgrade")
	}
	if goLine < 0 {
		t.Error("baselineStart does not launch the sampler goroutine")
	}
	if markLine > 0 && goLine > 0 && markLine > goLine {
		t.Errorf("the start mark is written at line %d but the goroutine starts at %d; "+
			"the mark has to land first or the first sample can precede it", markLine, goLine)
	}
}

// The sampler must return when its context is done. A ticker loop that ignores
// cancellation is a goroutine that never stops, and this provider is
// long-running and hot-swappable.
func TestBaselineSamplerStopsOnCancel(t *testing.T) {
	// Two cancel arms, counted rather than merely present: removing the one in
	// the ticker loop leaves the other, and that is the arm that matters, because
	// it is what stops the sampler when a hot swap promotes a new process.
	if got := countCancelCases(t, "baseline.go", "baselineRun"); got < 2 {
		t.Errorf("baselineRun has %d cancellation arm(s), want 2: one for the delayed "+
			"first sample and one in the ticker loop. Removing either leaves the sampler "+
			"running past shutdown and past a hot swap", got)
	}
	body, err := readFileString("baseline.go")
	if err != nil {
		t.Fatalf("read baseline.go: %v", err)
	}
	// The first sample must be delayed rather than taken at t=0, which would
	// measure a pool that has not launched yet, and that ramp is exactly what
	// compare excludes from both sides.
	if !strings.Contains(body, "NewTimer(") {
		t.Error("baselineRun takes no delayed first sample")
	}
	if !strings.Contains(body, "NewTicker(") {
		t.Error("baselineRun has no ticker; it cannot be sampling on an interval")
	}
}

// The wiring test above would pass on a file where the call exists only inside a
// string, so assert the parsed source is really the package's own and that the
// body it found is the one containing the other launchers.
func TestProvideFuncBodyIsTheRealOne(t *testing.T) {
	body := provideFuncBody(t)
	// provide() is where the sibling samplers are launched, so if this body does
	// not contain one of them it is the wrong block and the checks above are
	// looking somewhere meaningless.
	if body.firstIndex("runNodeSnapshotSampler") < 0 {
		t.Error("the parsed provide() body does not contain runNodeSnapshotSampler, so it " +
			"is not the block that launches the samplers; the checks above are void")
	}
	if body.firstIndex("baselineStart") < 0 {
		t.Error("baselineStart is not in the same body as the other samplers")
	}
}

// --- source-reading helpers ---

// baselineCall is one call expression found in a parsed body.
type baselineCall struct {
	line int
	args []baselineArg
	text string
}

type baselineArg struct{ text string }

// baselineBody is a parsed function body with helpers to ask what it calls.
type baselineBody struct {
	fset     *token.FileSet
	block    *ast.BlockStmt
	file     string
	funcName string
}

func (p baselineBody) firstIndex(name string) int {
	call := p.findCall(name)
	if call == nil {
		return -1
	}
	return call.line
}

func (p baselineBody) callWithFirstArg(name string) *baselineCall { return p.findCall(name) }

func (p baselineBody) findCall(name string) *baselineCall {
	var found *baselineCall
	ast.Inspect(p.block, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeIdent(call.Fun) != name {
			return true
		}
		found = &baselineCall{line: p.fset.Position(call.Pos()).Line, text: renderNode(p.fset, call.Fun)}
		for _, a := range call.Args {
			found.args = append(found.args, baselineArg{text: renderNode(p.fset, a)})
		}
		return false
	})
	return found
}

func (p baselineBody) enclosingFuncIs(name string) bool { return p.funcName == name }

// countCancelCases counts the `case <-ctx.Done():` arms in baselineRun's body.
// baselineRun has two: one guarding the delayed first sample and one in the
// ticker loop. Counting matters, because checking only for the presence of one
// would pass with the ticker loop's cancel removed, and that is the one that
// lets the sampler outlive a hot swap.
func countCancelCases(t *testing.T, file, funcName string) int {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	lines := strings.Split(string(src), "\n")
	startLine := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "func "+funcName+"(") {
			startLine = i
			break
		}
	}
	if startLine < 0 {
		t.Fatalf("%s not found in %s", funcName, file)
	}
	// Walk to the line that closes the function: the first line that is exactly
	// "}" at column zero.
	depth := 0
	n := 0
	for i := startLine; i < len(lines); i++ {
		depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		if strings.TrimSpace(lines[i]) == "case <-ctx.Done():" {
			n++
		}
		if i > startLine && depth == 0 {
			break
		}
	}
	return n
}

func provideFuncBody(t *testing.T) baselineBody {
	t.Helper()
	b := funcBodyFromFile(t, "main.go", "provide")
	if b == nil {
		t.Fatalf("provide() not found in provider/main.go")
	}
	return *b
}

func funcBodyFromFile(t *testing.T, file, name string) *baselineBody {
	t.Helper()
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, text, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		return &baselineBody{fset: fset, block: fn.Body, file: file, funcName: name}
	}
	return nil
}

// sourceLine returns the 1-based line of the first line containing needle, or
// -1. It is a plain text search on purpose: the point is to assert the ORDER of
// two statements in a function, which is a property of the source, not of the
// compiled behaviour.
func sourceLine(t *testing.T, file, needle string) int {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	return -1
}

func calleeIdent(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func renderNode(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
