package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiagRetentionIncludesNewBundle(t *testing.T) {
 env, _ := newDiagTestEnv(t)
 root := diagRoot(env)
 for _, name := range []string{"20261101T000000Z", "20261101T010000Z", "20261101T020000Z", "20261101T030000Z", "20261101T040000Z"} {
  if err := os.MkdirAll(filepath.Join(root,name), 0700); err != nil { t.Fatal(err) }
 }
 if _, err := runDiag(context.Background(), env); err != nil { t.Fatal(err) }
 if got := len(listDiagBundles(root)); got != env.keep { t.Fatalf("got %d bundles, want %d", got, env.keep) }
}

