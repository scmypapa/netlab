package files

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"netlab.local/core/api"
)

func Serve(w http.ResponseWriter, r *http.Request, store Store) {
	name := r.URL.Query().Get("path")
	if !strings.HasPrefix(name, "/") {
		Failure(w, errors.New("请选择绝对文件路径"), http.StatusBadRequest)
		return
	}
	content := strings.HasSuffix(r.URL.Path, "/content")
	var err error
	switch {
	case r.Method == http.MethodGet && content:
		var reader io.ReadCloser
		var size int64
		reader, size, err = store.Read(name)
		if err != nil {
			break
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		count, copyErr := io.Copy(w, reader)
		if copyErr != nil || count != size {
			slog.WarnContext(r.Context(), "file download interrupted", "error", copyErr, "bytes", count, "expected", size)
			panic(http.ErrAbortHandler)
		}
		return
	case r.Method == http.MethodGet:
		var entries []api.FileEntry
		entries, err = store.List(name)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			err = json.NewEncoder(w).Encode(entries)
		}
	case r.Method == http.MethodPut && content:
		err = store.Write(r.Context(), name, r.Body)
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodPost && !content:
		var command api.FileCommand
		if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&command); err != nil {
			Failure(w, err, http.StatusBadRequest)
			return
		}
		switch command.Action {
		case "mkdir":
			err = store.Mkdir(name)
		case "remove":
			err = store.Remove(name)
		case "rename":
			if command.Destination == nil || !strings.HasPrefix(*command.Destination, "/") {
				err = errors.New("请填写目标路径")
			} else {
				err = store.Rename(name, *command.Destination)
			}
		default:
			err = errors.New("文件操作无效")
		}
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		Failure(w, errors.New("文件操作无效"), http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		Failure(w, err, http.StatusUnprocessableEntity)
	}
}

func Failure(w http.ResponseWriter, err error, status int) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, os.ErrPermission):
		status = http.StatusForbidden
	case errors.Is(err, os.ErrExist):
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.Problem{Status: status, Title: http.StatusText(status), Detail: err.Error()})
}
