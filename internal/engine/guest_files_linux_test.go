//go:build linux

package engine

import (
	"context"
	"encoding/xml"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"netlab.local/core/api"
)

func TestInitializedGuestFilesDoNotRequireAgent(t *testing.T) {
	connection, err := libvirt.NewConnect("test:///default")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	instance := uuid.NewString()
	owner := Ownership{Environment: "environment", Asset: "vm", Instance: instance, GuestFilesInitialized: true}
	metadata, err := xml.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := connection.DomainDefineXML(fmt.Sprintf(`<domain type="test"><name>%s</name><uuid>%s</uuid><memory>524288</memory><vcpu>1</vcpu><os><type>hvm</type></os><metadata>%s</metadata></domain>`, instance, instance, metadata))
	if err != nil {
		t.Fatal(err)
	}
	defer domain.Free()
	defer domain.Undefine()
	vm := VirtualMachines{conn: connection}
	files := map[string]string{`C:\flag.txt`: "initial"}
	a := api.AssetExecution{InstanceId: instance, Template: api.Template{Os: "windows"},
		Asset: api.Asset{Id: "vm", Guest: &api.GuestSettings{Files: &files}}}
	if err = vm.initializeGuest(context.Background(), "environment", a); err != nil {
		t.Fatalf("initialized VM still requires its guest agent: %v", err)
	}
}
