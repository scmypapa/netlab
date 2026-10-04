package guest

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"netlab.local/core/api"
)

// Negotiate TLS without sending credentials, then present the peer certificate for confirmation.
func RDPCertificate(ctx context.Context, raw net.Conn) (string, error) {
	request := []byte{3, 0, 0, 19, 14, 224, 0, 0, 0, 0, 0, 1, 0, 8, 0, 3, 0, 0, 0}
	if _, err := raw.Write(request); err != nil {
		return "", err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(raw, header); err != nil {
		return "", err
	}
	length := int(binary.BigEndian.Uint16(header[2:]))
	if header[0] != 3 || length < 19 || length > 4096 {
		return "", errors.New("远程桌面 TLS 协商响应无效")
	}
	response := make([]byte, length-4)
	if _, err := io.ReadFull(raw, response); err != nil {
		return "", err
	}
	negotiation := response[len(response)-8:]
	if negotiation[0] != 2 || binary.LittleEndian.Uint32(negotiation[4:]) == 0 {
		return "", errors.New("远程桌面服务未提供 TLS")
	}
	connection := tls.Client(raw, &tls.Config{InsecureSkipVerify: true}) // No authenticated session is established by this probe.
	if err := connection.HandshakeContext(ctx); err != nil {
		return "", err
	}
	certificate := connection.ConnectionState().PeerCertificates[0]
	digest := sha256.Sum256(certificate.Raw)
	parts := make([]string, len(digest))
	for i, value := range digest {
		parts[i] = fmt.Sprintf("%02X", value)
	}
	return "sha256:" + strings.Join(parts, ":"), nil
}

func ValidateCertificate(fingerprint string) error {
	if !strings.HasPrefix(fingerprint, "sha256:") {
		return errors.New("请确认远程桌面证书")
	}
	value, err := hex.DecodeString(strings.ReplaceAll(strings.TrimPrefix(fingerprint, "sha256:"), ":", ""))
	if err != nil || len(value) != sha256.Size {
		return errors.New("远程桌面证书指纹无效")
	}
	return nil
}

func ValidateRDP(settings api.RDPSettings) error {
	if strings.TrimSpace(settings.Username) == "" || settings.Port < 1 || settings.Port > 65535 || settings.Password == nil || *settings.Password == "" {
		return errors.New("请填写用户名、密码和有效端口")
	}
	return ValidateCertificate(settings.Certificate)
}
