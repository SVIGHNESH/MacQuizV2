package blobstore

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestLocalSaveAndOpenRoundTrip(t *testing.T) {
	store := Local{Dir: t.TempDir(), Ext: ".csv"}

	ref, err := store.Save(context.Background(), strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasSuffix(ref, ".csv") || len(ref) != len(".csv")+32 {
		t.Fatalf("unexpected ref shape: %q", ref)
	}

	rc, err := store.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello" {
		t.Fatalf("round-tripped content mismatch: %q", got)
	}
}

func TestLocalPutOverwritesAndDeletes(t *testing.T) {
	dir := t.TempDir()
	store := Local{Dir: dir}

	if err := store.Put(context.Background(), "abcd1234.jpg", strings.NewReader("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(context.Background(), "abcd1234.jpg", strings.NewReader("v2")); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}

	rc, err := store.Open(context.Background(), "abcd1234.jpg")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v2" {
		t.Fatalf("overwrite not visible: %q", got)
	}

	// Put must not leave temp files behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the blob file in %s, got %d entries", dir, len(entries))
	}

	if err := store.Delete(context.Background(), "abcd1234.jpg"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(context.Background(), "abcd1234.jpg"); err != nil {
		t.Fatalf("Delete of missing ref should be idempotent: %v", err)
	}
	if _, err := store.Open(context.Background(), "abcd1234.jpg"); err == nil {
		t.Fatal("expected error opening deleted blob")
	}
}

func TestLocalRejectsPathTraversal(t *testing.T) {
	store := Local{Dir: t.TempDir()}

	for _, bad := range []string{"", "../secret.csv", "sub/dir.csv", "."} {
		if _, err := store.Open(context.Background(), bad); err == nil {
			t.Fatalf("Open(%q) = nil error, want rejection", bad)
		}
		if err := store.Put(context.Background(), bad, strings.NewReader("x")); err == nil {
			t.Fatalf("Put(%q) = nil error, want rejection", bad)
		}
		if err := store.Delete(context.Background(), bad); err == nil {
			t.Fatalf("Delete(%q) = nil error, want rejection", bad)
		}
	}
}
