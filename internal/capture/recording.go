package capture

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/singleflight"
	"netlab.local/core/api"
)

const PacketBudget int64 = 1 << 30
const partSize int64 = 64 << 20

type packetPart struct {
	ID         string                 `json:"id"`
	Start      time.Time              `json:"start"`
	End        time.Time              `json:"end"`
	Size       int64                  `json:"size"`
	Raw        int64                  `json:"raw"`
	Packets    int64                  `json:"packets"`
	Sealed     bool                   `json:"sealed"`
	Interfaces []api.CaptureInterface `json:"interfaces"`
	CaptureID  string                 `json:"captureId,omitempty"`
	Pinned     bool                   `json:"-"`
}
type environmentRecording struct {
	mu      sync.Mutex
	Quota   int64         `json:"quota"`
	Parts   []*packetPart `json:"parts"`
	active  *packetWriter
	writers map[string]*packetWriter
	running bool
	Error   string `json:"error,omitempty"`
}
type packetWriter struct {
	part       *packetPart
	file       *os.File
	compressed *zstd.Encoder
	packets    *pcapgo.NgWriter
}
type Recorder struct {
	mu              sync.RWMutex
	directory, node string
	environments    map[string]*environmentRecording
	analysisSlots   chan struct{}
	analyses        singleflight.Group
}
type compressedWriter struct {
	recorder    *Recorder
	environment string
	writer      *packetWriter
}

func NewRecorder(directory, node string) (*Recorder, error) {
	r := &Recorder{directory: filepath.Join(directory, "packets"), node: node, environments: map[string]*environmentRecording{}, analysisSlots: make(chan struct{}, max(1, min(4, runtime.NumCPU()/2)))}
	if err := os.MkdirAll(r.directory, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(r.directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err = uuid.Parse(entry.Name()); err != nil {
			return nil, err
		}
		raw, readErr := os.ReadFile(filepath.Join(r.directory, entry.Name(), "manifest.json"))
		if readErr != nil {
			return nil, readErr
		}
		var state environmentRecording
		if err = json.Unmarshal(raw, &state); err != nil {
			return nil, err
		}
		r.environments[entry.Name()] = &state
		known := map[string]bool{}
		for _, part := range state.Parts {
			known[part.ID+".pcapng.zst"] = true
			if !part.Sealed {
				if err = os.Truncate(r.filename(entry.Name(), part.ID), part.Size); err != nil {
					return nil, err
				}
			}
		}
		files, readErr := os.ReadDir(filepath.Join(r.directory, entry.Name()))
		if readErr != nil {
			return nil, readErr
		}
		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".pcapng.zst") && !known[file.Name()] {
				if err = os.Remove(filepath.Join(r.directory, entry.Name(), file.Name())); err != nil {
					return nil, err
				}
			}
		}
	}
	return r, nil
}
func (r *Recorder) save(environment string) error {
	raw, err := json.Marshal(r.environments[environment])
	if err != nil {
		return err
	}
	directory := filepath.Join(r.directory, environment)
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary := filepath.Join(directory, "manifest.tmp")
	if err = os.WriteFile(temporary, raw, 0600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(directory, "manifest.json"))
}

func (r *Recorder) lockRecording(environment string) (*environmentRecording, func()) {
	r.mu.RLock()
	state := r.environments[environment]
	if state != nil {
		state.mu.Lock()
	}
	return state, func() {
		if state != nil {
			state.mu.Unlock()
		}
		r.mu.RUnlock()
	}
}
func (r *Recorder) filename(environment, id string) string {
	return filepath.Join(r.directory, environment, id+".pcapng.zst")
}
func recordingSize(state *environmentRecording) int64 {
	var size int64
	for _, part := range state.Parts {
		size += part.Size
	}
	return size
}
func (r *Recorder) prune(environment string, limit int64, except string) error {
	state := r.environments[environment]
	for recordingSize(state) > limit {
		index := slices.IndexFunc(state.Parts, func(part *packetPart) bool {
			return part.ID != except && !part.Pinned && (state.active == nil || part.ID != state.active.part.ID)
		})
		if index < 0 {
			return ErrPacketQuota
		}
		part := state.Parts[index]
		if err := os.Remove(r.filename(environment, part.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		state.Parts = slices.Delete(state.Parts, index, index+1)
	}
	return nil
}
func (w compressedWriter) Write(data []byte) (int, error) {
	state := w.recorder.environments[w.environment]
	if err := w.recorder.prune(w.environment, state.Quota-int64(len(data)), w.writer.part.ID); err != nil {
		return 0, err
	}
	n, err := w.writer.file.Write(data)
	w.writer.part.Size += int64(n)
	return n, err
}
func (r *Recorder) closePart(environment string) error {
	state := r.environments[environment]
	writer := state.active
	if writer == nil {
		return nil
	}
	state.active = nil
	return r.finishWriter(environment, writer)
}

var ErrPacketQuota = errors.New("环境报文留存容量已用满")

func (r *Recorder) finishWriter(environment string, writer *packetWriter) error {
	err := writer.packets.Flush()
	err = errors.Join(err, writer.compressed.Close(), writer.file.Close())
	writer.part.Sealed = err == nil
	writer.part.Pinned = false
	delete(r.environments[environment].writers, writer.part.ID)
	return errors.Join(err, r.save(environment))
}
func (r *Recorder) startPart(environment string, interfaces []api.CaptureInterface, captureID string, at time.Time) (result *packetWriter, startErr error) {
	state := r.environments[environment]
	if state == nil || state.Quota == 0 {
		return nil, ErrPacketQuota
	}
	part := &packetPart{ID: uuid.NewString(), Start: at.UTC(), End: at.UTC(), Interfaces: interfaces, CaptureID: captureID, Pinned: captureID != ""}
	directory := filepath.Join(r.directory, environment)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(r.filename(environment, part.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	writer := &packetWriter{part: part, file: file}
	state.Parts = append(state.Parts, part)
	if state.writers == nil {
		state.writers = map[string]*packetWriter{}
	}
	state.writers[part.ID] = writer
	defer func() {
		if startErr != nil {
			file.Close()
			if writer.compressed != nil {
				writer.compressed.Close()
			}
			os.Remove(r.filename(environment, part.ID))
			delete(state.writers, part.ID)
			if index := slices.Index(state.Parts, part); index >= 0 {
				state.Parts = slices.Delete(state.Parts, index, index+1)
			}
		}
	}()
	encoder, err := zstd.NewWriter(compressedWriter{r, environment, writer}, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(256<<10))
	if err != nil {
		file.Close()
		return nil, err
	}
	writer.compressed = encoder
	pcap, err := pcapgo.NewNgWriterInterface(encoder, pcapInterface(interfaces[0]), pcapgo.DefaultNgWriterOptions)
	if err != nil {
		encoder.Close()
		file.Close()
		return nil, err
	}
	writer.packets = pcap
	for _, iface := range interfaces[1:] {
		if _, err = pcap.AddInterface(pcapInterface(iface)); err != nil {
			return nil, err
		}
	}
	return writer, nil
}
func (r *Recorder) configure(quotas map[string]int64, interfaces []api.SecurityInterface) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for environment, state := range r.environments {
		if _, assigned := quotas[environment]; !assigned {
			if err := r.remove(environment); err != nil {
				return err
			}
			continue
		}
		state.running = false
	}
	for environment, quota := range quotas {
		if _, err := uuid.Parse(environment); err != nil {
			return err
		}
		state := r.environments[environment]
		if state == nil {
			state = &environmentRecording{Parts: []*packetPart{}}
			r.environments[environment] = state
		}
		if quota < 0 || quota > PacketBudget {
			return errors.New("无效报文留存额度")
		}
		if state.Quota != quota {
			if err := r.closePart(environment); err != nil {
				return err
			}
			if err := r.prune(environment, quota, ""); err != nil {
				return err
			}
			state.Quota = quota
			state.Error = ""
			if err := r.save(environment); err != nil {
				return err
			}
		}
	}
	for _, iface := range interfaces {
		if state := r.environments[iface.EnvironmentId]; state != nil && iface.Interface.NodeId == r.node && state.Quota > 0 {
			state.running = true
		}
	}
	return nil
}
func (r *Recorder) Record(environment string, interfaces []api.CaptureInterface, index int, info gopacket.CaptureInfo, data []byte) error {
	state, release := r.lockRecording(environment)
	defer release()
	if state == nil {
		return nil
	}
	if state.Quota == 0 {
		return nil
	}
	rawLimit := min(partSize, max(int64(1024), state.Quota/4))
	if state.active != nil && (state.active.part.Raw+int64(len(data))+32 > rawLimit || info.Timestamp.Sub(state.active.part.Start) >= 30*time.Second) {
		if err := r.closePart(environment); err != nil {
			return err
		}
	}
	if state.active == nil {
		writer, err := r.startPart(environment, interfaces, "", info.Timestamp)
		if err != nil {
			return err
		}
		state.active = writer
	}
	writer := state.active
	info.InterfaceIndex = index
	if err := writer.packets.WritePacket(info, data); err != nil {
		return err
	}
	writer.part.Raw += int64(len(data)) + 32
	writer.part.Packets++
	writer.part.End = info.Timestamp.UTC()
	state.Error = ""
	return nil
}
func pcapInterface(iface api.CaptureInterface) pcapgo.NgInterface {
	return pcapgo.NgInterface{Name: iface.PortName, Description: iface.AssetName + " / " + iface.InterfaceId, LinkType: layers.LinkTypeEthernet, TimestampResolution: 9}
}
func (r *Recorder) interfaceStatistics(environment, port string, stats pcapgo.NgInterfaceStatistics) error {
	state, release := r.lockRecording(environment)
	defer release()
	if state == nil {
		return nil
	}
	if state.active == nil {
		return nil
	}
	index := slices.IndexFunc(state.active.part.Interfaces, func(iface api.CaptureInterface) bool { return iface.PortName == port })
	if index < 0 {
		return nil
	}
	return state.active.packets.WriteInterfaceStats(index, stats)
}
func (r *Recorder) Flush() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result error
	for environment, state := range r.environments {
		state.mu.Lock()
		err := r.flushEnvironment(environment)
		state.mu.Unlock()
		result = errors.Join(result, err)
	}
	return result
}
func (r *Recorder) flushEnvironment(environment string) error {
	state := r.environments[environment]
	if state == nil || len(state.writers) == 0 {
		return nil
	}
	var err error
	for _, writer := range state.writers {
		err = errors.Join(err, writer.packets.Flush(), writer.compressed.Flush())
	}
	if err != nil {
		state.Error = err.Error()
	}
	return errors.Join(err, r.save(environment))
}
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	for environment := range r.environments {
		err = errors.Join(err, r.closePart(environment))
	}
	return err
}
func (r *Recorder) Remove(environment string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remove(environment)
}
func (r *Recorder) remove(environment string) error {
	if state := r.environments[environment]; state != nil {
		if err := r.closePart(environment); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(r.directory, environment)); err != nil {
		return err
	}
	delete(r.environments, environment)
	return nil
}
func (r *Recorder) Retention() map[string]api.PacketRetention {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := map[string]api.PacketRetention{}
	for environment, state := range r.environments {
		state.mu.Lock()
		result[environment] = recordingRetention(state)
		state.mu.Unlock()
	}
	return result
}

func (r *Recorder) retentionFor(environment string) api.PacketRetention {
	state, release := r.lockRecording(environment)
	defer release()
	if state == nil {
		return api.PacketRetention{}
	}
	return recordingRetention(state)
}

func recordingRetention(state *environmentRecording) api.PacketRetention {
	status := api.PacketRetentionStatusIdle
	if state.running {
		status = api.PacketRetentionStatusRecording
	}
	info := api.PacketRetention{LimitBytes: state.Quota, UsedBytes: recordingSize(state), Status: status}
	for _, part := range state.Parts {
		if info.OldestAt == nil || part.Start.Before(*info.OldestAt) {
			at := part.Start
			info.OldestAt = &at
		}
		if info.NewestAt == nil || part.End.After(*info.NewestAt) {
			at := part.End
			info.NewestAt = &at
		}
	}
	if state.Error != "" {
		message := state.Error
		info.Error = &message
		info.Status = api.PacketRetentionStatusUnavailable
	}
	return info
}
func packetIdentity(node, part string, frame int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(node + "/" + part + "/" + strconv.FormatInt(frame, 10)))
}
func ParsePacketIdentity(value string) (node, part string, frame int64, err error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", "", 0, errors.New("无效报文标识")
	}
	values := strings.Split(string(data), "/")
	if len(values) != 3 {
		return "", "", 0, errors.New("无效报文标识")
	}
	if _, err = uuid.Parse(values[0]); err != nil {
		return "", "", 0, err
	}
	if _, err = uuid.Parse(values[1]); err != nil {
		return "", "", 0, err
	}
	frame, err = strconv.ParseInt(values[2], 10, 64)
	if err != nil || frame < 1 {
		return "", "", 0, errors.New("无效帧编号")
	}
	return values[0], values[1], frame, nil
}
func packetCursor(packet api.CapturePacket) string {
	return base64.RawURLEncoding.EncodeToString([]byte(packet.Time.Format(time.RFC3339Nano) + "/" + *packet.Id))
}

type packetPosition struct {
	at time.Time
	id string
}

func parsePacketCursor(cursor string) (packetPosition, error) {
	if cursor == "" {
		return packetPosition{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return packetPosition{}, fmt.Errorf("无效报文游标")
	}
	at, id, ok := strings.Cut(string(raw), "/")
	if !ok {
		return packetPosition{}, fmt.Errorf("无效报文游标")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return packetPosition{}, err
	}
	return packetPosition{timestamp, id}, nil
}
func packetBefore(packet api.CapturePacket, cursor packetPosition) bool {
	return cursor.id == "" || packet.Time.Before(cursor.at) || packet.Time.Equal(cursor.at) && *packet.Id < cursor.id
}

type partRead struct {
	part   packetPart
	file   *os.File
	reader *zstd.Decoder
}

func (r *Recorder) openParts(environment string) ([]partRead, error) {
	state, release := r.lockRecording(environment)
	defer release()
	if state == nil {
		return []partRead{}, nil
	}
	if err := r.flushEnvironment(environment); err != nil {
		return nil, err
	}
	result := []partRead{}
	for _, part := range state.Parts {
		result = append(result, partRead{part: *part})
	}
	return result, nil
}
func (r *Recorder) openPart(environment string, part *partRead) error {
	state, release := r.lockRecording(environment)
	defer release()
	if state == nil {
		return os.ErrNotExist
	}
	file, err := os.Open(r.filename(environment, part.part.ID))
	if err != nil {
		return err
	}
	decoder, err := zstd.NewReader(io.NewSectionReader(file, 0, part.part.Size), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20))
	if err != nil {
		file.Close()
		return err
	}
	part.file, part.reader = file, decoder
	return nil
}
func closeParts(parts []partRead) {
	for _, part := range parts {
		if part.reader != nil {
			part.reader.Close()
		}
		if part.file != nil {
			part.file.Close()
		}
	}
}
