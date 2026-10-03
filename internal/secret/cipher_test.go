package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentControllersShareDurableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "secret.key")
	keys := make([]*Cipher, 16)
	var group sync.WaitGroup
	for i := range keys {
		group.Go(func() {
			var err error
			keys[i], err = OpenFile(path)
			if err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	for _, key := range keys {
		if key == nil {
			t.Fatal("key creation failed")
		}
	}
	value := []byte(`{"username":"test","password":"test-secret"}`)
	sealed := keys[0].Encrypt(value, "template/1")
	if bytes.Contains(sealed, value) {
		t.Fatal("credentials stored as plaintext")
	}
	for _, key := range keys {
		actual, err := key.Decrypt(sealed, "template/1")
		if err != nil || !bytes.Equal(actual, value) {
			t.Fatalf("shared key: %v", err)
		}
	}
	for _, input := range [][]byte{sealed[:8], append([]byte{}, sealed...)} {
		input[len(input)-1] ^= 1
		if _, err := keys[0].Decrypt(input, "template/1"); err == nil {
			t.Fatal("modified credentials accepted")
		}
	}
	if _, err := keys[0].Decrypt(sealed, "another-template/1"); err == nil {
		t.Fatal("credentials from another template accepted")
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path); err == nil {
		t.Fatal("invalid key accepted")
	}
}
