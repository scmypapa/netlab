package logfile

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, text string
		lines      int
		want       string
	}{
		{"terminated", "one\ntwo\nthree\n", 2, "two\nthree\n"},
		{"unterminated", "one\ntwo\nthree", 2, "two\nthree"},
		{"all", "one\ntwo", 200, "one\ntwo"},
		{"none", "one\ntwo", 0, ""},
		{"empty", "", 2, ""},
		{"huge-line", strings.Repeat("x", 2*MaxTailBytes), 2, strings.Repeat("x", MaxTailBytes)},
		{"huge-unicode-line", strings.Repeat("中", MaxTailBytes/3+40), 2, strings.Repeat("中", MaxTailBytes/3)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdout.log")
			if err := os.WriteFile(path, []byte(test.text), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err = SeekTail(file, test.lines); err != nil {
				t.Fatal(err)
			}
			actual, err := io.ReadAll(file)
			if err != nil || string(actual) != test.want {
				t.Fatalf("tail length=%d, wanted %d; error=%v", len(actual), len(test.want), err)
			}
		})
	}
}

func TestLogOptions(t *testing.T) {
	for _, raw := range []string{"tail=-1", "tail=10001", "tail=abc", "stream=vm", "follow=perhaps"} {
		query, _ := url.ParseQuery(raw)
		if _, err := Parse(query); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	query, _ := url.ParseQuery("tail=0&stream=stderr&follow=true")
	options, err := Parse(query)
	if err != nil || options != (Options{Tail: 0, Stream: "stderr", Follow: true}) {
		t.Fatalf("options=%+v error=%v", options, err)
	}
}
