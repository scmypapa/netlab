package logfile

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
)

const MaxTailBytes = 1 << 20

type Options struct {
	Tail   int
	Stream string
	Follow bool
}

func Parse(query url.Values) (Options, error) {
	options := Options{Tail: 200, Stream: "all"}
	var err error
	if value := query.Get("tail"); value != "" {
		options.Tail, err = strconv.Atoi(value)
		if err != nil || options.Tail < 0 || options.Tail > 10000 {
			return options, fmt.Errorf("tail 应在 0–10000 之间")
		}
	}
	if value := query.Get("stream"); value != "" {
		options.Stream = value
	}
	if options.Stream != "all" && options.Stream != "stdout" && options.Stream != "stderr" {
		return options, fmt.Errorf("stream 应为 all、stdout 或 stderr")
	}
	if value := query.Get("follow"); value != "" {
		options.Follow, err = strconv.ParseBool(value)
		if err != nil {
			return options, fmt.Errorf("follow 应为 true 或 false")
		}
	}
	return options, nil
}

// SeekTail scans only the final bounded window, then positions the file at the
// requested line. Existing unterminated output counts as the final line.
func SeekTail(file *os.File, lines int) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if lines == 0 {
		_, err = file.Seek(size, io.SeekStart)
		return size, err
	}
	start := max(int64(0), size-MaxTailBytes)
	window := make([]byte, size-start)
	if _, err = file.ReadAt(window, start); err != nil && err != io.EOF {
		return size, err
	}
	end := len(window)
	if end > 0 && window[end-1] == '\n' {
		end--
	}
	for line := 0; line < lines; line++ {
		previous := bytes.LastIndexByte(window[:end], '\n')
		if previous < 0 {
			end = 0
			break
		}
		end = previous
		if line == lines-1 {
			end++
			break
		}
	}
	_, err = file.Seek(start+int64(end), io.SeekStart)
	return size, err
}
