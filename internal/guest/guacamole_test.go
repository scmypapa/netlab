package guest

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func TestGuacamoleFraming(t *testing.T) {
	values := []string{"clipboard", "中文;🙂", "", "a,b.c"}
	wire := GuacInstruction(values...)
	reader := bufio.NewReader(strings.NewReader(wire + GuacInstruction("sync", "123")))
	decoded, err := ReadGuacInstruction(reader)
	if err != nil || !reflect.DeepEqual(decoded, values) {
		t.Fatalf("%v: %v", decoded, err)
	}
	decoded, err = ReadGuacInstruction(reader)
	if err != nil || !reflect.DeepEqual(decoded, []string{"sync", "123"}) {
		t.Fatalf("%v: %v", decoded, err)
	}
	for _, invalid := range []string{"-1.x;", "65537.x;", "a.x;", "1.a!", "2.a;"} {
		if _, err = ReadGuacInstruction(bufio.NewReader(strings.NewReader(invalid))); err == nil {
			t.Errorf("accepted malformed instruction %q", invalid)
		}
	}
}

func TestRDPCertificateFormat(t *testing.T) {
	if err := ValidateCertificate("sha256:" + strings.Repeat("AB", 32)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCertificate("sha256:" + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "sha256:AB", "sha256:" + strings.Repeat("Z", 64)} {
		if ValidateCertificate(value) == nil {
			t.Errorf("accepted invalid certificate %q", value)
		}
	}
}
