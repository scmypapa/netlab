package operation

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

func (w Worker) removeCaptures(ctx context.Context, environment string) error {
	refs, err := w.Queries.ListCaptureSegments(ctx, environment)
	if err != nil {
		return err
	}
	nodes := map[string]string{}
	for _, ref := range refs {
		nodes[ref.NodeID] = ref.Endpoint
	}
	var mutex sync.Mutex
	var waiting sync.WaitGroup
	for _, endpoint := range nodes {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			removeErr := w.Client.Do(deadline, http.MethodDelete, endpoint, "/node/v1/environments/"+url.PathEscape(environment)+"/captures", nil, nil)
			mutex.Lock()
			err = errors.Join(err, removeErr)
			mutex.Unlock()
		}()
	}
	waiting.Wait()
	if err == nil {
		err = w.Queries.DeleteEnvironmentCaptures(ctx, environment)
	}
	return err
}
