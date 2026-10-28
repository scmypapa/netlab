//go:build linux

package capture

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"netlab.local/core/api"
)

type analysisWaiter struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *analysisWaiter) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func waitAnalysis(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-deadline.C:
			t.Fatal("packet analysis did not reach the expected process state")
		case <-ticker.C:
		}
	}
}

func TestRealRecentAnalysisSharingAndCancellation(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CAPTURE") != "1" {
		t.Skip("set NETLAB_REAL_CAPTURE=1 for native TShark test")
	}
	program, err := exec.LookPath("tshark")
	if err != nil {
		t.Fatal(err)
	}
	for _, last := range []bool{false, true} {
		name := "one reader remains"
		if last {
			name = "last reader leaves"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			started, release := filepath.Join(directory, "started"), filepath.Join(directory, "release")
			// Delay the real executable at its process boundary, without replacing its parsing.
			wrapper := "#!/bin/sh\nprintf '%s\\n' \"$$\" >> \"$NETLAB_TEST_TSHARK_STARTED\"\nwhile [ ! -e \"$NETLAB_TEST_TSHARK_RELEASE\" ]; do sleep 0.01; done\nexec \"$NETLAB_TEST_TSHARK\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(directory, "tshark"), []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("NETLAB_TEST_TSHARK", program)
			t.Setenv("NETLAB_TEST_TSHARK_STARTED", started)
			t.Setenv("NETLAB_TEST_TSHARK_RELEASE", release)
			r, environment, interfaces := recordingFixture(t, 1<<20)
			r.analysisSlots = make(chan struct{}, 1)
			frame := sampleFrame(t)
			if err := r.Record(environment, interfaces, 0, gopacket.CaptureInfo{Timestamp: time.Now(), Length: len(frame), CaptureLength: len(frame)}, frame); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			query := RecentQuery{Limit: 10}
			first := make(chan error, 1)
			go func() { _, err := r.Recent(ctx, environment, query); first <- err }()
			waitAnalysis(t, func() bool { _, err := os.Stat(started); return err == nil })
			type result struct {
				page api.RecentPacketPage
				err  error
			}
			second := make(chan result, 1)
			if !last {
				ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				waiter := &analysisWaiter{Context: ctx, joined: make(chan struct{})}
				go func() { page, err := r.Recent(waiter, environment, query); second <- result{page, err} }()
				select {
				case <-waiter.joined:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			cancel()
			select {
			case err := <-first:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancelled viewer did not leave", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled viewer remained blocked")
			}
			if last {
				waitAnalysis(t, func() bool { return len(r.analysisSlots) == 0 })
			} else if len(r.analysisSlots) != 1 {
				t.Fatal("one reader cancelled another reader's analysis")
			}
			data, err := os.ReadFile(started)
			if err != nil || len(strings.Fields(string(data))) != 1 {
				t.Fatalf("same query launched duplicate parser processes: %q, %v", data, err)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if last {
				ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				page, err := r.Recent(ctx, environment, RecentQuery{Limit: 10, Protocol: "UDP"})
				if err != nil || len(page.Packets) != 1 {
					t.Fatalf("new filter could not acquire released capacity: %+v, %v", page, err)
				}
			} else {
				select {
				case result := <-second:
					if result.err != nil || len(result.page.Packets) != 1 {
						t.Fatalf("remaining reader lost the real packet: %+v, %v", result.page, result.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("remaining reader did not complete")
				}
			}
		})
	}
}
