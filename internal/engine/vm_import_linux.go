//go:build linux

package engine

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"netlab.local/core/api"
)

type ovfEnvelope struct {
	Files []struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	} `xml:"References>File"`
	Disks []struct {
		ID       string `xml:"diskId,attr"`
		File     string `xml:"fileRef,attr"`
		Capacity string `xml:"capacity,attr"`
		Units    string `xml:"capacityAllocationUnits,attr"`
	} `xml:"DiskSection>Disk"`
	Systems []struct {
		OS       string `xml:"OperatingSystemSection>Description"`
		Hardware struct {
			Machine string      `xml:"System>VirtualSystemType"`
			Items   []ovfItem   `xml:"Item"`
			Configs []ovfConfig `xml:"Config"`
		} `xml:"VirtualHardwareSection"`
		Configs []ovfConfig `xml:"Config"`
		Extra   []ovfConfig `xml:"ExtraConfig"`
	} `xml:"VirtualSystem"`
}
type ovfConfig struct {
	Key   string `xml:"key,attr"`
	Value string `xml:"value,attr"`
}
type ovfItem struct {
	ID       string `xml:"InstanceID"`
	Type     int    `xml:"ResourceType"`
	Subtype  string `xml:"ResourceSubType"`
	Quantity string `xml:"VirtualQuantity"`
	Units    string `xml:"AllocationUnits"`
	Parent   string `xml:"Parent"`
	Address  *int   `xml:"Address"`
	Unit     *int   `xml:"AddressOnParent"`
	Host     string `xml:"HostResource"`
	Boot     int    `xml:"BootOrder"`
}
type importDisk struct {
	definition api.TemplateDisk
	path       string
	origin     string
	capacity   int64
}

func artifactPath(root, relative string) (string, error) {
	if !filepath.IsLocal(relative) || relative == "." || strings.Contains(relative, "\\") {
		return "", fmt.Errorf("artifact path %q must stay inside the template", relative)
	}
	return filepath.Join(root, relative), nil
}

func artifactReference(source, relative string) (string, error) {
	base, err := url.Parse(source)
	if err != nil {
		return "", err
	}
	if base.Scheme == "" {
		return filepath.Join(filepath.Dir(source), relative), nil
	}
	reference, err := url.Parse(relative)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(reference).String(), nil
}

var vmdkExtent = regexp.MustCompile(`(?m)^(?:RW|RDONLY|NOACCESS)\s+\d+\s+\w+\s+"([^"]+)"`)

func prepareVMDKExtents(ctx context.Context, path, origin string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 512)
	n, err := file.ReadAt(header, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	var descriptor []byte
	if n >= 44 && string(header[:4]) == "KDMV" {
		offset, sectors := binary.LittleEndian.Uint64(header[28:36]), binary.LittleEndian.Uint64(header[36:44])
		if sectors == 0 {
			return nil
		}
		if sectors > 2048 || offset > uint64(1<<63-1)/512 {
			return errors.New("VMDK descriptor exceeds supported size")
		}
		descriptor = make([]byte, sectors*512)
		if _, err = file.ReadAt(descriptor, int64(offset*512)); err != nil {
			return err
		}
	} else if strings.HasPrefix(strings.TrimSpace(string(header[:n])), "# Disk DescriptorFile") {
		descriptor, err = io.ReadAll(io.LimitReader(file, 1<<20))
		if err != nil {
			return err
		}
	} else {
		return nil
	}
	for _, extent := range vmdkExtent.FindAllSubmatch(descriptor, -1) {
		relative := string(extent[1])
		destination, err := artifactPath(filepath.Dir(path), relative)
		if err != nil {
			return err
		}
		if _, err = os.Stat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) || origin == "" {
			return err
		}
		source, err := artifactReference(origin, relative)
		if err != nil {
			return err
		}
		if err = copyArtifact(ctx, source, destination); err != nil {
			return err
		}
	}
	return nil
}

func copyArtifact(ctx context.Context, source, destination string) error {
	u, err := url.Parse(source)
	if err != nil {
		return err
	}
	var reader io.ReadCloser
	switch u.Scheme {
	case "", "file":
		path := source
		if u.Scheme == "file" {
			path = u.Path
		}
		reader, err = os.Open(path)
	case "http", "https":
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err == nil {
			var response *http.Response
			response, err = http.DefaultClient.Do(req)
			if err == nil {
				reader = response.Body
				if response.StatusCode != http.StatusOK {
					reader.Close()
					return fmt.Errorf("artifact download: HTTP %d", response.StatusCode)
				}
			}
		}
	default:
		return fmt.Errorf("unsupported artifact source scheme %q", u.Scheme)
	}
	if err != nil {
		return err
	}
	defer reader.Close()
	if err = os.MkdirAll(filepath.Dir(destination), 0711); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, reader)
	return errors.Join(copyErr, file.Close())
}

func extractOVA(source, destination string) (string, error) {
	file, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer file.Close()
	archive := tar.NewReader(file)
	var descriptor string
	for {
		entry, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		path, err := artifactPath(destination, entry.Name)
		if err != nil {
			return "", err
		}
		switch entry.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0711)
		case tar.TypeReg, tar.TypeRegA:
			if err = os.MkdirAll(filepath.Dir(path), 0711); err != nil {
				return "", err
			}
			var output *os.File
			output, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
			if err == nil {
				_, copyErr := io.Copy(output, archive)
				err = errors.Join(copyErr, output.Close())
			}
			if strings.EqualFold(filepath.Ext(path), ".ovf") {
				if descriptor != "" {
					return "", errors.New("a VM template imports one OVF descriptor")
				}
				descriptor = path
			}
		default:
			return "", fmt.Errorf("OVA entry %q is not a regular file or directory", entry.Name)
		}
		if err != nil {
			return "", err
		}
	}
	if descriptor == "" {
		return "", errors.New("OVA contains no OVF descriptor")
	}
	return descriptor, nil
}

func allocationBytes(quantity, units string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(quantity), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid OVF quantity %q", quantity)
	}
	multiplier := int64(1)
	unit := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(units), " ", ""))
	switch unit {
	case "", "byte", "bytes":
	case "kb", "kilobytes":
		multiplier = 1000
	case "mb", "megabytes":
		multiplier = 1000 * 1000
	case "gb", "gigabytes":
		multiplier = 1000 * 1000 * 1000
	case "kib":
		multiplier = 1 << 10
	case "mib":
		multiplier = 1 << 20
	case "gib":
		multiplier = 1 << 30
	default:
		var base, power int
		if _, err = fmt.Sscanf(unit, "byte*%d^%d", &base, &power); err != nil || base < 2 || power < 0 || power > 63 {
			return 0, fmt.Errorf("unsupported OVF allocation units %q", units)
		}
		for range power {
			if multiplier > (1<<63-1)/int64(base) {
				return 0, fmt.Errorf("OVF allocation units %q exceed supported size", units)
			}
			multiplier *= int64(base)
		}
	}
	if n > (1<<63-1)/multiplier {
		return 0, errors.New("OVF disk or memory exceeds supported size")
	}
	return n * multiplier, nil
}

func ovfDisks(descriptor string, t api.Template) (api.Template, []importDisk, map[string]string, error) {
	file, err := os.Open(descriptor)
	if err != nil {
		return t, nil, nil, err
	}
	defer file.Close()
	var document ovfEnvelope
	if err = xml.NewDecoder(file).Decode(&document); err != nil {
		return t, nil, nil, err
	}
	if len(document.Systems) != 1 {
		return t, nil, nil, errors.New("a VM template imports one OVF VirtualSystem")
	}
	system := document.Systems[0]
	hardware := api.Hardware{}
	if t.Hardware != nil {
		hardware = *t.Hardware
	}
	configs := append(append(system.Configs, system.Extra...), system.Hardware.Configs...)
	for _, config := range configs {
		switch strings.ToLower(config.Key) {
		case "firmware":
			switch strings.ToLower(config.Value) {
			case "bios":
				hardware.Firmware = api.Bios
			case "efi", "uefi":
				hardware.Firmware = api.Uefi
			default:
				return t, nil, nil, fmt.Errorf("OVF firmware %q is unsupported", config.Value)
			}
		case "uefi.secureboot.enabled", "efi.secureboot.enabled":
			value, err := strconv.ParseBool(config.Value)
			if err != nil {
				return t, nil, nil, fmt.Errorf("OVF secure boot: %w", err)
			}
			hardware.SecureBoot = &value
		case "cpumodel":
			hardware.CpuModel = ptr(config.Value)
		case "machine":
			hardware.Machine = config.Value
		}
	}
	if strings.HasPrefix(system.Hardware.Machine, "pc-") || system.Hardware.Machine == "pc" || system.Hardware.Machine == "q35" {
		hardware.Machine = system.Hardware.Machine
	}
	if hardware.Machine == "" {
		if hardware.Firmware == api.Uefi {
			hardware.Machine = "q35"
		} else if hardware.Firmware == api.Bios {
			hardware.Machine = "pc"
		}
	}
	controllers := map[string]api.TemplateDisk{}
	controllerIndex := map[api.TemplateDiskBus]int{}
	attachments := map[string]ovfItem{}
	nics := []string{}
	for _, item := range system.Hardware.Items {
		subtype := strings.ToLower(item.Subtype)
		switch item.Type {
		case 3:
			t.Resources.Cpu, err = strconv.Atoi(strings.TrimSpace(item.Quantity))
			if err != nil || t.Resources.Cpu < 1 {
				return t, nil, nil, fmt.Errorf("OVF vCPU quantity %q is invalid", item.Quantity)
			}
		case 4:
			units := item.Units
			if units == "" {
				units = "byte * 2^20"
			}
			memory, err := allocationBytes(item.Quantity, units)
			if err != nil {
				return t, nil, nil, err
			}
			t.Resources.MemoryMiB = (memory + (1 << 20) - 1) / (1 << 20)
		case 5, 6, 20:
			controller := api.TemplateDisk{}
			switch {
			case item.Type == 5:
				controller.Bus = api.Ide
			case strings.Contains(subtype, "sata") || strings.Contains(subtype, "ahci"):
				controller.Bus = api.Sata
			case item.Type == 6:
				controller.Bus = api.Scsi
				models := map[string]string{"lsilogic": "lsilogic", "lsilogicsas": "lsisas1068", "lsisas1068": "lsisas1068", "virtualscsi": "vmpvscsi", "vmware.pvscsi": "vmpvscsi", "vmpvscsi": "vmpvscsi", "virtio-scsi": "virtio-scsi"}
				model := models[subtype]
				if model == "" {
					return t, nil, nil, fmt.Errorf("OVF SCSI controller %q is unsupported", item.Subtype)
				}
				controller.ControllerModel = &model
			default:
				return t, nil, nil, fmt.Errorf("OVF storage controller %q is unsupported", item.Subtype)
			}
			index := controllerIndex[controller.Bus]
			if item.Address != nil {
				index = *item.Address
			}
			if index < 0 {
				return t, nil, nil, fmt.Errorf("OVF controller %s has a negative address", item.ID)
			}
			controllerIndex[controller.Bus] = max(controllerIndex[controller.Bus], index+1)
			controller.ControllerIndex = &index
			controllers[item.ID] = controller
		case 10:
			models := map[string]api.HardwareNicModel{"e1000": api.HardwareNicModelE1000, "e1000e": api.HardwareNicModelE1000e, "virtio": api.HardwareNicModelVirtio, "virtio-net": api.HardwareNicModelVirtio, "rtl8139": api.HardwareNicModelRtl8139, "vmxnet3": api.HardwareNicModelVmxnet3}
			model, ok := models[subtype]
			if !ok {
				return t, nil, nil, fmt.Errorf("OVF network adapter %q is unsupported", item.Subtype)
			}
			nics = append(nics, string(model))
		case 17:
			attachments[strings.TrimPrefix(item.Host, "ovf:/disk/")] = item
		}
	}
	if len(nics) > 0 {
		hardware.NicModel = api.HardwareNicModel(nics[0])
		t.NicModels = &nics
	}
	if hardware.Firmware == "" || hardware.Machine == "" || hardware.NicModel == "" {
		return t, nil, nil, errors.New("OVF has no complete firmware, machine or NIC description; register the missing virtual hardware")
	}
	if system.OS != "" {
		t.Os = system.OS
	}
	files := make(map[string]string, len(document.Files))
	for _, file := range document.Files {
		reference, err := url.Parse(file.Href)
		if err != nil || reference.Scheme != "" || reference.Host != "" {
			return t, nil, nil, fmt.Errorf("OVF image reference %q must be relative to the descriptor", file.Href)
		}
		if _, err = artifactPath(filepath.Dir(descriptor), reference.Path); err != nil {
			return t, nil, nil, err
		}
		files[file.ID] = reference.Path
	}
	disks := make([]importDisk, 0, len(document.Disks))
	identities, orders := map[string]bool{}, map[int]bool{}
	for index, disk := range document.Disks {
		file, ok := files[disk.File]
		if disk.File != "" && !ok {
			return t, nil, nil, fmt.Errorf("OVF disk %s has no image reference", disk.ID)
		}
		definition := api.TemplateDisk{Id: disk.ID, Bus: api.TemplateDiskBus(hardware.DiskBus), BootOrder: index + 1, ControllerModel: hardware.DiskController}
		if attachment, ok := attachments[disk.ID]; ok {
			controller, ok := controllers[attachment.Parent]
			if !ok {
				return t, nil, nil, fmt.Errorf("OVF disk %s refers to missing controller %s", disk.ID, attachment.Parent)
			}
			definition.Bus, definition.ControllerModel, definition.ControllerIndex = controller.Bus, controller.ControllerModel, controller.ControllerIndex
			if attachment.Unit != nil {
				if *attachment.Unit < 0 {
					return t, nil, nil, fmt.Errorf("OVF disk %s has a negative controller slot", disk.ID)
				}
				definition.ControllerUnit = attachment.Unit
			}
			if attachment.Boot > 0 {
				definition.BootOrder = attachment.Boot
			}
		}
		if definition.Bus == "" {
			return t, nil, nil, fmt.Errorf("OVF disk %s has no disk controller description", disk.ID)
		}
		if definition.Id == "" || identities[definition.Id] || orders[definition.BootOrder] {
			return t, nil, nil, fmt.Errorf("OVF disk %s repeats a disk identity or boot order", definition.Id)
		}
		identities[definition.Id], orders[definition.BootOrder] = true, true
		var capacity int64
		if disk.Capacity != "" {
			capacity, err = allocationBytes(disk.Capacity, disk.Units)
			if err != nil {
				return t, nil, nil, err
			}
		}
		path := ""
		if disk.File != "" {
			path = filepath.Join(filepath.Dir(descriptor), file)
		} else if capacity == 0 {
			return t, nil, nil, fmt.Errorf("OVF empty disk %s has no capacity", disk.ID)
		}
		disks = append(disks, importDisk{definition: definition, path: path, origin: file, capacity: capacity})
	}
	if len(disks) == 0 {
		return t, nil, nil, errors.New("OVF contains no system disk images")
	}
	sort.SliceStable(disks, func(i, j int) bool { return disks[i].definition.BootOrder < disks[j].definition.BootOrder })
	hardware.DiskBus = api.HardwareDiskBus(disks[0].definition.Bus)
	hardware.DiskController = disks[0].definition.ControllerModel
	t.Hardware = &hardware
	return t, disks, files, nil
}

func (v *VirtualMachines) prepareTemplate(ctx context.Context, t api.Template) (api.Template, error) {
	if !filepath.IsLocal(t.Id) || t.Id == "." || strings.ContainsAny(t.Id, "/\\") {
		return t, errors.New("template id must be a single stable identifier")
	}
	key := fmt.Sprintf("%s/%d", t.Id, t.Version)
	lock, _ := v.sources.LoadOrStore(key, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	directory := filepath.Join(v.data, "artifacts", key)
	if data, err := os.ReadFile(filepath.Join(directory, "template.json")); err == nil {
		err = json.Unmarshal(data, &t)
		return t, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return t, err
	}
	if err := os.MkdirAll(filepath.Dir(directory), 0711); err != nil {
		return t, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(directory), "import-")
	if err != nil {
		return t, err
	}
	defer os.RemoveAll(staging)
	if err = os.Chmod(staging, 0711); err != nil {
		return t, err
	}
	u, err := url.Parse(t.Source)
	if err != nil {
		return t, err
	}
	format := strings.ToLower(strings.TrimPrefix(filepath.Ext(u.Path), "."))
	if t.Format != nil {
		format = string(*t.Format)
	}
	if format == "iso" {
		return t, errors.New("an ISO is installation media; prepare an installed system disk before publishing a VM template")
	}
	name := filepath.Base(u.Path)
	if name == "." || name == "/" {
		name = "image"
	}
	input := filepath.Join(staging, "input", name)
	remote := u.Scheme == "http" || u.Scheme == "https"
	if u.Scheme == "" || u.Scheme == "file" {
		path := t.Source
		if u.Scheme == "file" {
			path = u.Path
		}
		input, err = filepath.Abs(path)
		if err != nil {
			return t, err
		}
	} else if err = copyArtifact(ctx, t.Source, input); err != nil {
		return t, err
	}
	var disks []importDisk
	if format == "ova" || format == "ovf" {
		descriptor := input
		if format == "ova" {
			descriptor, err = extractOVA(input, filepath.Join(staging, "input", "package"))
			if err != nil {
				return t, err
			}
		}
		t, disks, _, err = ovfDisks(descriptor, t)
		if err != nil {
			return t, err
		}
		if format == "ovf" {
			copied := map[string]bool{}
			for index := range disks {
				if disks[index].path != "" {
					disks[index].origin, err = artifactReference(t.Source, disks[index].origin)
					if err != nil {
						return t, err
					}
					if remote && !copied[disks[index].path] {
						if err = copyArtifact(ctx, disks[index].origin, disks[index].path); err != nil {
							return t, err
						}
						copied[disks[index].path] = true
					}
				}
			}
		} else {
			for index := range disks {
				disks[index].origin = ""
			}
		}
		data, err := os.ReadFile(descriptor)
		if err != nil {
			return t, err
		}
		if err = os.WriteFile(filepath.Join(staging, "source.ovf"), data, 0640); err != nil {
			return t, err
		}
	} else {
		if t.Hardware == nil {
			return t, errors.New("bare VM disk imports need virtual hardware")
		}
		disks = []importDisk{{definition: api.TemplateDisk{Id: "boot", Bus: api.TemplateDiskBus(t.Hardware.DiskBus), BootOrder: 1, ControllerModel: t.Hardware.DiskController}, path: input, origin: t.Source}}
	}
	t, err = v.pinHardware(t)
	if err != nil {
		return t, err
	}
	definitions := make([]api.TemplateDisk, len(disks))
	t.Resources.DiskGiB = 0
	for index, disk := range disks {
		if disk.path == "" {
			disk.path = filepath.Join(staging, "input", fmt.Sprintf("empty-%d.qcow2", index))
			if err = os.MkdirAll(filepath.Dir(disk.path), 0711); err != nil {
				return t, err
			}
			if err = command(ctx, "qemu-img", "create", "-f", "qcow2", disk.path, fmt.Sprint(disk.capacity)); err != nil {
				return t, err
			}
		}
		if err = prepareVMDKExtents(ctx, disk.path, disk.origin); err != nil {
			return t, fmt.Errorf("disk %s: %w", disk.definition.Id, err)
		}
		image, err := inspectImage(ctx, disk.path)
		if err != nil {
			return t, fmt.Errorf("disk %s: %w", disk.definition.Id, err)
		}
		if image.BackingFilename != "" {
			return t, fmt.Errorf("disk %s has an external backing disk; import a flattened disk image", disk.definition.Id)
		}
		if image.Format != "qcow2" && image.Format != "raw" && image.Format != "vmdk" {
			return t, fmt.Errorf("VM disk format %q is unsupported", image.Format)
		}
		if disk.capacity > 0 && disk.capacity != image.VirtualSize {
			return t, fmt.Errorf("OVF disk %s declares %d bytes but its image contains %d bytes", disk.definition.Id, disk.capacity, image.VirtualSize)
		}
		disk.definition.SizeGiB = (image.VirtualSize + (1 << 30) - 1) / (1 << 30)
		definitions[index] = disk.definition
		t.Resources.DiskGiB += disk.definition.SizeGiB
		if err = command(ctx, "qemu-img", "convert", "-f", image.Format, "-O", "qcow2", disk.path, filepath.Join(staging, fmt.Sprintf("disk-%d.qcow2", index))); err != nil {
			return t, fmt.Errorf("disk %s: %w", disk.definition.Id, err)
		}
	}
	t.Disks = &definitions
	data, err := json.Marshal(t)
	if err != nil {
		return t, err
	}
	if err = os.WriteFile(filepath.Join(staging, "template.json"), data, 0640); err != nil {
		return t, err
	}
	if err = os.RemoveAll(filepath.Join(staging, "input")); err != nil {
		return t, err
	}
	if err = os.Rename(staging, directory); err != nil {
		return t, err
	}
	return t, nil
}
