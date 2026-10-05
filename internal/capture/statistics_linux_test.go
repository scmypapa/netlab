//go:build linux

package capture

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureInterfaceDrops(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(order.String(), func(t *testing.T) {
			block := func(kind uint32, body []byte) []byte {
				result := make([]byte, len(body)+12)
				order.PutUint32(result, kind)
				order.PutUint32(result[4:], uint32(len(result)))
				copy(result[8:], body)
				order.PutUint32(result[len(result)-4:], uint32(len(result)))
				return result
			}
			section := make([]byte, 16)
			order.PutUint32(section, 0x1a2b3c4d)
			order.PutUint16(section[4:], 1)
			stats := func(iface uint32, count uint64) []byte {
				body := make([]byte, 28)
				order.PutUint32(body, iface)
				order.PutUint16(body[12:], 5)
				order.PutUint16(body[14:], 8)
				order.PutUint64(body[16:], count)
				return block(5, body)
			}
			path := filepath.Join(t.TempDir(), "capture.pcapng")
			data := block(0x0a0d0d0a, section)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if drops, err := captureDrops(path); err != nil || drops != nil {
				t.Fatalf("absent statistics: %v %v", drops, err)
			}
			data = append(data, stats(0, 3)...)
			data = append(data, stats(0, 7)...)
			data = append(data, stats(1, 2)...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if drops, err := captureDrops(path); err != nil || drops == nil || *drops != 9 {
				t.Fatalf("cumulative interface statistics: %v %v", drops, err)
			}
			if err := os.WriteFile(path, data[:len(data)-1], 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := captureDrops(path); err == nil {
				t.Fatal("truncated capture accepted")
			}
		})
	}
}
