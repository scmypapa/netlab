package engine

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGuestFileInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested space", "flag'quoted")
	content := "flag{你好}\n$HOME `touch ignored` $(false)\n"
	request := guestFileCommand(runtime.GOOS, path, content)
	input, err := base64.StdEncoding.DecodeString(request["input-data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(request["path"].(string), request["arg"].([]string)...)
	command.Stdin = strings.NewReader(string(input))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("write: %s: %v", output, err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != content {
		t.Fatalf("guest file content mismatch: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0644 {
		t.Fatalf("guest file permissions: %v, %v", info, err)
	}
}
