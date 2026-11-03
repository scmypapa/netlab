package transport

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const artifactLengthHeader = "X-Netlab-Artifact-Length"

// Content-Length describes compressed bytes; backups need the decoded archive length.
func ArtifactLength(response *http.Response) (int64, error) {
	length := response.ContentLength
	if value := response.Header.Get(artifactLengthHeader); value != "" {
		var err error
		length, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid artifact length %q: %w", value, err)
		}
		if length < 0 {
			return 0, errors.New("artifact length must be nonnegative")
		}
	}
	return length, nil
}

// Go's node transport negotiates gzip and decodes it while streaming to disk.
func WriteArtifact(w http.ResponseWriter, r *http.Request, reader io.Reader, length int64) error {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set(artifactLengthHeader, strconv.FormatInt(length, 10))
	w.Header().Set("Vary", "Accept-Encoding")
	if r.Header.Get("Accept-Encoding") == "gzip" {
		w.Header().Set("Content-Encoding", "gzip")
		compressed, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			return err
		}
		_, err = io.CopyN(compressed, reader, length)
		return errors.Join(err, compressed.Close())
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	_, err := io.CopyN(w, reader, length)
	return err
}
