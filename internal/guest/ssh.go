package guest

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"
	"netlab.local/core/api"
)

func Authentication(settings api.SSHSettings) (ssh.AuthMethod, error) {
	switch settings.AuthKind {
	case "password":
		if settings.Password == nil || *settings.Password == "" {
			return nil, errors.New("请填写 SSH 密码")
		}
		return ssh.Password(*settings.Password), nil
	case "key":
		if settings.PrivateKey == nil || *settings.PrivateKey == "" {
			return nil, errors.New("请填写 SSH 私钥")
		}
		var key ssh.Signer
		var err error
		if settings.Passphrase != nil && *settings.Passphrase != "" {
			key, err = ssh.ParsePrivateKeyWithPassphrase([]byte(*settings.PrivateKey), []byte(*settings.Passphrase))
		} else {
			key, err = ssh.ParsePrivateKey([]byte(*settings.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("SSH 私钥无效：%w", err)
		}
		return ssh.PublicKeys(key), nil
	default:
		return nil, errors.New("请选择密码或私钥认证")
	}
}

func Connect(raw net.Conn, address string, settings api.SSHSettings) (*ssh.Client, error) {
	auth, err := Authentication(settings)
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{User: settings.Username, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		actual := ssh.FingerprintSHA256(key)
		if actual != settings.HostKey {
			return fmt.Errorf("SSH 主机密钥已变化：%s", actual)
		}
		return nil
	}}
	connection, channels, requests, err := ssh.NewClientConn(raw, address, config)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(connection, channels, requests), nil
}

func HostKey(raw net.Conn, address string) (string, error) {
	var fingerprint string
	presented := errors.New("host key presented")
	_, _, _, err := ssh.NewClientConn(raw, address, &ssh.ClientConfig{User: "netlab", HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fingerprint = ssh.FingerprintSHA256(key)
		return presented
	}})
	if !errors.Is(err, presented) {
		return "", err
	}
	return fingerprint, nil
}

func Validate(settings api.SSHSettings) error {
	if strings.TrimSpace(settings.Username) == "" || settings.Port < 1 || settings.Port > 65535 {
		return errors.New("请填写用户名和有效端口")
	}
	if !strings.HasPrefix(settings.HostKey, "SHA256:") {
		return errors.New("请确认 SSH 主机密钥")
	}
	_, err := Authentication(settings)
	return err
}
