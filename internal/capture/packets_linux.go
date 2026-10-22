//go:build linux

package capture

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"netlab.local/core/api"
)

func (m *Manager) Packets(ctx context.Context, id, environment string, query PacketQuery) (api.CapturePacketPage, error) {
	result := api.CapturePacketPage{Packets: []api.CapturePacket{}}
	detail, err := m.Get(id, environment)
	if err != nil {
		return result, err
	}
	file, err := os.Open(filepath.Join(m.directory, id, "capture.pcapng"))
	if err != nil {
		return result, err
	}
	defer file.Close()
	err = readFrames(ctx, file, detail.Segment.Status == "running" || detail.Segment.Status == "starting", func(packet api.CapturePacket, _ []byte) bool {
		if packet.Number <= query.After || !query.matches(packet) {
			return true
		}
		if len(result.Packets) == query.Limit {
			cursor := result.Packets[len(result.Packets)-1].Number
			result.NextCursor = &cursor
			return false
		}
		result.Packets = append(result.Packets, packet)
		return true
	})
	return result, err
}

func (m *Manager) Packet(ctx context.Context, id, environment string, number int64) (api.CapturePacketDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := api.CapturePacketDetail{Layers: []api.PacketField{}}
	detail, err := m.Get(id, environment)
	if err != nil {
		return result, err
	}
	path := filepath.Join(m.directory, id, "capture.pcapng")
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	err = readFrames(ctx, file, detail.Segment.Status == "running" || detail.Segment.Status == "starting", func(packet api.CapturePacket, data []byte) bool {
		if packet.Number != number {
			return true
		}
		result.Packet, result.Hex = packet, hex.EncodeToString(data)
		return false
	})
	if err != nil {
		return result, err
	}
	if result.Packet.Number == 0 {
		return result, os.ErrNotExist
	}
	command := exec.CommandContext(ctx, "tshark", "-n", "-r", path, "-c", strconv.FormatInt(number, 10), "-Y", "frame.number == "+strconv.FormatInt(number, 10), "-T", "pdml")
	var output struct {
		Packets []struct {
			Protocols []pdmlField `xml:"proto"`
		} `xml:"packet"`
	}
	data, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return result, fmt.Errorf("报文解析失败：%s", exit.Stderr)
		}
		return result, err
	}
	if err = xml.Unmarshal(data, &output); err != nil {
		return result, err
	}
	if len(output.Packets) != 1 {
		return result, fmt.Errorf("解析结果中未找到帧 %d", number)
	}
	for _, field := range output.Packets[0].Protocols {
		result.Layers = append(result.Layers, field.model())
	}
	return result, nil
}

type pdmlField struct {
	Name     string      `xml:"name,attr"`
	Label    string      `xml:"showname,attr"`
	Show     string      `xml:"show,attr"`
	Offset   int         `xml:"pos,attr"`
	Length   int         `xml:"size,attr"`
	Children []pdmlField `xml:",any"`
}

func (field pdmlField) model() api.PacketField {
	result := api.PacketField{Name: field.Name, Label: first(field.Label, field.Name), Offset: field.Offset, Length: field.Length, Children: []api.PacketField{}}
	if field.Show != "" {
		result.Value = &field.Show
	}
	for _, child := range field.Children {
		result.Children = append(result.Children, child.model())
	}
	return result
}
