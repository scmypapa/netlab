//go:build linux

package capture

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// Interface statistics are cumulative. Read block headers and statistics, never packet payloads.
func captureDrops(path string) (*int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	var order binary.ByteOrder = binary.LittleEndian
	counts := map[uint64]uint64{}
	section := uint64(0)
	for offset := int64(0); offset < info.Size(); {
		var header [12]byte
		if _, err = file.ReadAt(header[:], offset); err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(header[:4]) == 0x0a0d0d0a {
			switch binary.LittleEndian.Uint32(header[8:]) {
			case 0x1a2b3c4d:
				order = binary.LittleEndian
			case 0x4d3c2b1a:
				order = binary.BigEndian
			default:
				return nil, errors.New("invalid pcapng byte order")
			}
			section++
		}
		size := int64(order.Uint32(header[4:8]))
		if size < 12 || size%4 != 0 || offset+size > info.Size() {
			return nil, io.ErrUnexpectedEOF
		}
		if order.Uint32(header[:4]) == 5 {
			// ISB contains a 12-byte body followed by small options and the block trailer.
			if size < 24 || size > 65536 {
				return nil, errors.New("invalid pcapng interface statistics")
			}
			body := make([]byte, size-12)
			if _, err = file.ReadAt(body, offset+8); err != nil {
				return nil, err
			}
			key := section<<32 | uint64(order.Uint32(body[:4]))
			for at := 12; at+4 <= len(body); {
				code, length := order.Uint16(body[at:]), int(order.Uint16(body[at+2:]))
				at += 4
				if code == 0 {
					break
				}
				if at+length > len(body) {
					return nil, io.ErrUnexpectedEOF
				}
				if code == 5 && length == 8 {
					counts[key] = order.Uint64(body[at : at+length])
				}
				at += (length + 3) &^ 3
			}
		}
		offset += size
	}
	if len(counts) == 0 {
		return nil, nil
	}
	var total int64
	for _, count := range counts {
		total += int64(count)
	}
	return &total, nil
}
