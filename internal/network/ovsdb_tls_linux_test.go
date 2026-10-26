//go:build linux

package network

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/client"
	"netlab.local/core/api"
)

func TestRealOVNMutualTLS(t *testing.T) {
	if os.Getenv("NETLAB_REAL_NETWORK") != "1" {
		t.Skip("set NETLAB_REAL_NETWORK=1 for isolated OVN TLS test")
	}
	directory := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Netlab isolated TLS test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(directory, "test.crt"), filepath.Join(directory, "test.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(directory, "nb.db")
	if output, err := exec.Command("ovsdb-tool", "create", database, "/usr/share/ovn/ovn-nb.ovsschema").CombinedOutput(); err != nil {
		t.Fatalf("create isolated database: %s %v", output, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	listener.Close()
	logFile, err := os.Create(filepath.Join(directory, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	start := func() *exec.Cmd {
		server := exec.Command("ovsdb-server", "--remote=pssl:"+port+":127.0.0.1", "--private-key="+keyPath, "--certificate="+certPath, "--ca-cert="+certPath, "--pidfile="+filepath.Join(directory, "server.pid"), "--unixctl=none", "--no-chdir", database)
		server.Stdout, server.Stderr = logFile, logFile
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		return server
	}
	server := start()
	defer func() { server.Process.Kill(); server.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	ovn, err := NewOVN(ctx, "ssl:"+address, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}})
	if err != nil {
		output, _ := os.ReadFile(filepath.Join(directory, "server.log"))
		t.Fatalf("%v\n%s", err, output)
	}
	defer ovn.Close()
	var switches []Switch
	if err := ovn.client.List(ctx, &switches); err != nil {
		t.Fatal(err)
	}
	plan := api.NodePlan{EnvironmentId: "reconnect", Spec: api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "10.10.0.0/24"}}}}
	if err := ovn.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	waitSwitch := func() string {
		for {
			var current []Switch
			if err := ovn.client.List(ctx, &current); err != nil {
				t.Fatal(err)
			}
			if len(current) == 1 && current[0].Name == objectName("ls", plan.EnvironmentId, "lan") {
				return current[0].UUID
			}
			select {
			case <-ctx.Done():
				t.Fatal("monitor did not refresh after reconnect")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	previousUUID := waitSwitch()
	for range 2 {
		if err := server.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		server.Wait()
		for !errors.Is(ovn.client.Echo(ctx), client.ErrNotConnected) {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		attempt, done := context.WithTimeout(ctx, 100*time.Millisecond)
		err := ovn.Remove(attempt, plan.EnvironmentId)
		done()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("disconnected transaction must honor its deadline, got %v", err)
		}
		server = start()
		if err := ovn.Apply(ctx, plan); err != nil {
			t.Fatalf("apply after server restart: %v", err)
		}
		if actual := waitSwitch(); actual != previousUUID {
			t.Fatalf("reconnecting replaced the existing logical switch: before=%s after=%s", previousUUID, actual)
		}
	}
}
