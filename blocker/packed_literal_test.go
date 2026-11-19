package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func TestEmitPackedHostDataSingleLiteral(t *testing.T) {
	for _, keys := range [][]uint64{nil, {0x010203040506, 0x0708090a0b0c}} {
		source := emit("standard", "pepper", keys, nil, nil, nil)
		file, err := parser.ParseFile(token.NewFileSet(), "generated.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if value.Names[0].Name != "blockerBlockedHostData" {
					continue
				}
				found = true
				lit, ok := value.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("host data is not a single string literal: %T", value.Values[0])
				}
				got, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				want := ""
				if len(keys) != 0 {
					want = "\x02\x03\x04\x05\x06\x08\x09\x0a\x0b\x0c"
				}
				if got != want {
					t.Fatalf("host bytes = %x, want %x", got, want)
				}
			}
		}
		if !found {
			t.Fatal("missing host data constant")
		}
	}
}
