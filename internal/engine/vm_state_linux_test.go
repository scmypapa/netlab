//go:build linux

package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTPMArchivePreservesIdentityAndExcludesRuntimeLocks(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "restored")
	state := filepath.Join(source, "tpm2")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tpm2-00.permall", ".lock"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte("persisted TPM keys"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(root, "tpm.tar")
	if err := archiveTPM(source, archive); err != nil {
		t.Fatal(err)
	}
	if err := restoreTPM(archive, destination); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "tpm2", "tpm2-00.permall"))
	if err != nil || string(content) != "persisted TPM keys" {
		t.Fatalf("restored state: %q, %v", content, err)
	}
	before, _ := os.Stat(filepath.Join(state, "tpm2-00.permall"))
	after, _ := os.Stat(filepath.Join(destination, "tpm2", "tpm2-00.permall"))
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatal("TPM file permissions changed during transfer")
	}
	if _, err := os.Stat(filepath.Join(destination, "tpm2", ".lock")); !os.IsNotExist(err) {
		t.Fatal("runtime lock was included in persistent state")
	}
}
