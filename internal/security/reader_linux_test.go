//go:build linux

package security

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"netlab.local/core/api"
)

func TestEVEReaderRotationPartialWriteAndReplay(t *testing.T) {
	m := &Manager{directory: t.TempDir(), node: "one"}
	name := "20261022T100000.000000000"
	directory := filepath.Join(m.directory, name)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	iface := peer("blue", "web", "tap-blue", "one")
	run := &sensorRun{Devices: []attachment{{Device: "tap-blue", SecurityInterface: iface}}, Peers: []api.SecurityInterface{iface}, directory: directory}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "interfaces.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	line := eve("tap-blue", "10.0.0.2", "10.0.0.3")
	first := filepath.Join(directory, "eve-2026102210.json")
	if err = os.WriteFile(first, append(append([]byte{}, line...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	result := api.NodeSecurityObservation{}
	if err = m.read(context.Background(), "", run, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Alerts) != 1 || result.HistoryGap {
		t.Fatalf("%+v", result)
	}
	committed := result.Cursor
	second := filepath.Join(directory, "eve-2026102211.json")
	if err = os.WriteFile(second, line, 0600); err != nil {
		t.Fatal(err)
	}
	partial := api.NodeSecurityObservation{Cursor: committed}
	if err = m.read(context.Background(), committed, run, &partial); err != nil {
		t.Fatal(err)
	}
	if len(partial.Alerts) != 0 || partial.Cursor != committed {
		t.Fatalf("partial record advanced cursor: %+v", partial)
	}
	if err = os.WriteFile(second, append(append([]byte{}, line...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		replay := api.NodeSecurityObservation{Cursor: committed}
		if err = m.read(context.Background(), committed, run, &replay); err != nil {
			t.Fatal(err)
		}
		if len(replay.Alerts) != 1 || replay.Cursor == committed {
			t.Fatalf("uncommitted batch not replayed: %+v", replay)
		}
	}
	if err = os.Remove(first); err != nil {
		t.Fatal(err)
	}
	gap := api.NodeSecurityObservation{Cursor: committed}
	if err = m.read(context.Background(), committed, run, &gap); err != nil {
		t.Fatal(err)
	}
	if !gap.HistoryGap || len(gap.Alerts) != 1 {
		t.Fatalf("missing file not reported: %+v", gap)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err = os.Chtimes(second, old, old); err != nil {
		t.Fatal(err)
	}
	if err = m.prune(run); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("expired EVE retained: %v", err)
	}
	if _, err = os.Stat(directory); err != nil {
		t.Fatalf("active metadata removed: %v", err)
	}
}
