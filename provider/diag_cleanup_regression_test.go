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

func TestDiagOrphanCleanupPreservesUnrelatedPartial(t *testing.T) {
 env, _ := newDiagTestEnv(t)
 root := diagRoot(env)
 for _, name := range []string{"20261102T000000Z.partial", "operator-notes.partial"} {
  if err := os.MkdirAll(filepath.Join(root,name), 0700); err != nil { t.Fatal(err) }
 }
 removeDiagOrphans(root)
 if _, err := os.Stat(filepath.Join(root,"operator-notes.partial")); err != nil { t.Fatalf("unrelated data removed: %v",err) }
 if _, err := os.Stat(filepath.Join(root,"20261102T000000Z.partial")); !os.IsNotExist(err) { t.Fatalf("orphan survived: %v",err) }
}

