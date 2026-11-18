package connect

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestGeneratedBlockerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	cmd := exec.Command("timeout", "900", "go", "test", "-count=1", "-run", "^TestGeneratedBucketFixture$", "./blocker")
	cmd.Env = append(os.Environ(), "BLOCKER_ROUNDTRIP_FIXTURE="+path, "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate bucket fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Source   string
		Blocked  []string
		Excluded []string
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "generated.go", fixture.Source, 0)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			v := spec.(*ast.ValueSpec)
			lit, ok := v.Values[0].(*ast.BasicLit)
			if !ok {
				t.Fatalf("unexpected generated value for %s", v.Names[0].Name)
			}
			if lit.Kind == token.STRING {
				values[v.Names[0].Name], err = strconv.Unquote(lit.Value)
			} else {
				values[v.Names[0].Name] = lit.Value
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	count, err := strconv.Atoi(values["blockerBlockedHostCount"])
	if err != nil {
		t.Fatal(err)
	}
	b, err := newBucketBlockerWithData(values["blockerPepper"], values["blockerBlockedHostBuckets"], values["blockerBlockedHostData"], count, "", 0, "", 0)
	if err != nil {
		t.Fatalf("checked decoder rejected generated table: %v", err)
	}
	b.SetEnabled(true)
	for _, host := range fixture.Blocked {
		if !b.BlockHost(host) || !b.BlockHost("sub."+host) {
			t.Errorf("generated table did not block %q and its subdomain", host)
		}
	}
	if len(fixture.Excluded) == 0 {
		t.Fatal("fixture did not exercise resolver exclusions")
	}
	for _, host := range append(fixture.Excluded, "unlisted.example.com", "safe.example.net", "other.example.org") {
		if b.BlockHost(host) {
			t.Errorf("generated table unexpectedly blocks %q", host)
		}
	}
}
