package guest

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"netlab.local/core/api"
)

func GuacInstruction(values ...string) string {
	var output strings.Builder
	for i, value := range values {
		if i > 0 {
			output.WriteByte(',')
		}
		fmt.Fprintf(&output, "%d.%s", utf8.RuneCountInString(value), value)
	}
	output.WriteByte(';')
	return output.String()
}

func ReadGuacInstruction(reader *bufio.Reader) ([]string, error) {
	var values []string
	for {
		length, err := reader.ReadSlice('.')
		if err != nil {
			return nil, err
		}
		count, err := strconv.Atoi(string(length[:len(length)-1]))
		if err != nil || count < 0 || count > 65536 {
			return nil, fmt.Errorf("invalid Guacamole element length")
		}
		var value strings.Builder
		for i := 0; i < count; i++ {
			char, size, err := reader.ReadRune()
			if err != nil {
				return nil, err
			}
			if char == utf8.RuneError && size == 1 {
				return nil, fmt.Errorf("invalid Guacamole UTF-8")
			}
			value.WriteRune(char)
		}
		values = append(values, value.String())
		separator, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if separator == ';' {
			return values, nil
		}
		if separator != ',' {
			return nil, fmt.Errorf("invalid Guacamole separator")
		}
	}
}

func OpenRDP(connection io.ReadWriter, reader *bufio.Reader, hostname, port string, settings api.RDPSettings, width, height, dpi int) (string, error) {
	if _, err := io.WriteString(connection, GuacInstruction("select", "rdp")); err != nil {
		return "", err
	}
	args, err := ReadGuacInstruction(reader)
	if err != nil {
		return "", err
	}
	if len(args) < 2 || args[0] != "args" {
		return "", fmt.Errorf("Guacamole did not provide connection parameters")
	}
	parameters := map[string]string{
		"VERSION_1_5_0": "VERSION_1_5_0", "hostname": hostname, "port": port,
		"username": settings.Username, "password": *settings.Password, "security": "any",
		"cert-fingerprints": settings.Certificate, "resize-method": "display-update", "color-depth": "32",
		"disable-audio": "true", "enable-drive": "false", "enable-printing": "false", "enable-sftp": "false",
		"disable-upload": "true", "disable-download": "true", "enable-font-smoothing": "true",
	}
	if settings.Domain != nil {
		parameters["domain"] = *settings.Domain
	}
	connect := []string{"connect"}
	for _, name := range args[1:] {
		connect = append(connect, parameters[name])
	}
	request := GuacInstruction("size", strconv.Itoa(width), strconv.Itoa(height), strconv.Itoa(dpi)) +
		GuacInstruction("audio") + GuacInstruction("video") + GuacInstruction("image", "image/png", "image/jpeg") + GuacInstruction(connect...)
	if _, err = io.WriteString(connection, request); err != nil {
		return "", err
	}
	ready, err := ReadGuacInstruction(reader)
	if err != nil {
		return "", err
	}
	if len(ready) < 2 || ready[0] != "ready" {
		return "", fmt.Errorf("Guacamole connection failed: %v", ready)
	}
	return ready[1], nil
}
