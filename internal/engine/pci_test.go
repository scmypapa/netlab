package engine

import "testing"

func TestPCIHostDevices(t *testing.T) {
	devices, err := domainHostDevices([]string{"0000:03:00.0", "0000:03:00.1"})
	if err != nil || len(devices) != 2 {
		t.Fatal(devices, err)
	}
	for i, device := range devices {
		address := device.SubsysPCI.Source.Address
		if device.Managed != "yes" || *address.Domain != 0 || *address.Bus != 3 || *address.Slot != 0 || *address.Function != uint(i) {
			t.Fatalf("multifunction group lost: %+v", device)
		}
	}
	for _, invalid := range []string{"0000::03:00.0", "0000:03:00:0", "0000:03:20.0", "0000:03:00.8", "0000:gg:00.0", "../device"} {
		if _, err := domainHostDevices([]string{invalid}); err == nil {
			t.Fatal("invalid BDF accepted", invalid)
		}
	}
}
