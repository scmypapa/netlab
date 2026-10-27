package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeRuntimeDependencies(t *testing.T) {
	for _, tc := range []struct {
		name, virtual, extra, module, failure, wantInstall string
		missingKmod                                        bool
		missingLogrotate                                   bool
	}{
		{name: "complete generic kernel", module: "ready"},
		{name: "log retention dependency", module: "ready", missingLogrotate: true, wantInstall: "logrotate"},
		{name: "virtual current and next kernels", virtual: "installed linux-image-virtual 6.8.0-147.147", wantInstall: "linux-modules-extra-6.8.0-142-generic linux-image-extra-virtual=6.8.0-147.147"},
		{name: "virtual future kernel dependency", virtual: "installed linux-image-virtual 6.8.0-147.147", module: "ready", wantInstall: "linux-image-extra-virtual=6.8.0-147.147"},
		{name: "HWE virtual keeps its series", virtual: "installed linux-image-virtual-hwe-24.04 6.14.0-36.36~24.04.1", module: "ready", wantInstall: "linux-image-extra-virtual-hwe-24.04=6.14.0-36.36~24.04.1"},
		{name: "installed metapackage", virtual: "installed linux-image-virtual 6.8.0-147.147", extra: "installed 6.8.0-147.147", module: "ready"},
		{name: "outdated metapackage", virtual: "installed linux-image-virtual 6.8.0-147.147", extra: "installed 6.8.0-142.142", module: "ready", wantInstall: "linux-image-extra-virtual=6.8.0-147.147"},
		{name: "newer metapackage is not downgraded", virtual: "installed linux-image-virtual 6.8.0-142.142", extra: "installed 6.8.0-147.147", module: "ready"},
		{name: "removed metapackage is not installed", virtual: "config-files linux-image-virtual 6.8.0-147.147", module: "ready"},
		{name: "kmod is prepared", missingKmod: true, wantInstall: "kmod linux-modules-extra-6.8.0-142-generic"},
		{name: "apt update failure", failure: "apt-update"},
		{name: "apt install failure", failure: "apt-install", wantInstall: "linux-modules-extra-6.8.0-142-generic"},
		{name: "module failure", module: "ready", failure: "modprobe"},
		{name: "persistence failure", module: "ready", failure: "install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, module := fakeNodeRuntime(t, tc.missingKmod)
			if tc.missingLogrotate {
				if err := os.Remove(filepath.Join(filepath.Dir(log), "logrotate")); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TEST_VIRTUAL", tc.virtual)
			t.Setenv("TEST_EXTRA", tc.extra)
			t.Setenv("TEST_MODULE", tc.module)
			t.Setenv("TEST_FAILURE", tc.failure)
			command := exec.Command("bash", "../../scripts/install-security.sh")
			output, err := command.CombinedOutput()
			if (err != nil) != (tc.failure != "") {
				t.Fatalf("err=%v, output=%s", err, output)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantInstall != "" {
				if !strings.Contains(string(calls), "apt-get install -y "+tc.wantInstall+"\n") {
					t.Fatalf("wrong dependency installation:\n%s", calls)
				}
			} else if strings.Contains(string(calls), "apt-get ") && tc.failure != "apt-update" {
				t.Fatalf("reinstalled available dependencies:\n%s", calls)
			}
			if tc.failure != "" {
				if strings.Contains(string(calls), "systemctl ") {
					t.Fatalf("continued after failed dependency preparation:\n%s", calls)
				}
				if _, err := os.Stat(module); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("persisted failed module preparation: %v", err)
				}
			} else {
				data, err := os.ReadFile(module)
				if err != nil || string(data) != "vrf\n" {
					t.Fatalf("module persistence=%q err=%v", data, err)
				}
				if !strings.Contains(string(calls), "systemctl enable --now netlab-suricata-rules.timer\n") {
					t.Fatalf("security dependency preparation was lost:\n%s", calls)
				}
			}
		})
	}
}

func TestNodeUpdatePreparesRuntimeBeforeSwitch(t *testing.T) {
	for _, failure := range []string{"", "modprobe", "missing hook"} {
		t.Run(failure, func(t *testing.T) {
			_, module := fakeNodeRuntime(t, false)
			t.Setenv("TEST_MODULE", "ready")
			t.Setenv("TEST_FAILURE", failure)
			cfg := Config{Role: "node", InstallDir: t.TempDir(), DataDir: t.TempDir()}
			destination := filepath.Join(cfg.InstallDir, "releases", "v1.1.1")
			for _, name := range []string{"netlab-controller", "netlab-node", "restic", "web/index.html", "victoria-metrics-prod", "guacamole/sbin/guacd", "guacamole/lib/libguac-client-rdp.so"} {
				writeRuntimeTestFile(t, filepath.Join(destination, name), []byte("fixture"), 0600)
			}
			writeRuntimeTestFile(t, filepath.Join(destination, "release.json"), []byte(`{"version":"v1.1.1","os":"linux","arch":"amd64"}`), 0600)
			for _, name := range []string{"install-security.sh", "install-node-runtime.sh"} {
				if failure == "missing hook" && name == "install-security.sh" {
					continue
				}
				script, err := os.ReadFile(filepath.Join("..", "..", "scripts", name))
				if err != nil {
					t.Fatal(err)
				}
				writeRuntimeTestFile(t, filepath.Join(destination, "scripts", name), script, 0700)
			}
			current := filepath.Join(cfg.InstallDir, "releases", "v1.1.0")
			if err := os.Symlink(current, filepath.Join(cfg.InstallDir, "current")); err != nil {
				t.Fatal(err)
			}
			writeRuntimeTestFile(t, filepath.Join(cfg.DataDir, "update", "request.json"), []byte(`{"Version":"v1.1.1"}`), 0600)
			reachedSwitch := false
			stop := errors.New("stop before service switch")
			err := Execute(context.Background(), cfg, "", nil, func(context.Context, string) (func(error) error, error) {
				reachedSwitch = true
				if data, err := os.ReadFile(module); err != nil || string(data) != "vrf\n" {
					t.Fatalf("switch reached before runtime preparation: %q %v", data, err)
				}
				return nil, stop
			})
			if failure == "" {
				if !reachedSwitch || !errors.Is(err, stop) {
					t.Fatalf("did not prepare before switch: reached=%v err=%v", reachedSwitch, err)
				}
			} else if reachedSwitch || err == nil || !strings.Contains(err.Error(), "准备节点运行依赖") {
				t.Fatalf("failed dependencies did not block switch: reached=%v err=%v", reachedSwitch, err)
			}
			actual, err := os.Readlink(filepath.Join(cfg.InstallDir, "current"))
			if err != nil || actual != current {
				t.Fatalf("running release changed: %s %v", actual, err)
			}
		})
	}
}

func fakeNodeRuntime(t *testing.T, missingKmod bool) (string, string) {
	t.Helper()
	directory := t.TempDir()
	log, module := filepath.Join(directory, "calls"), filepath.Join(directory, "modules.conf")
	dispatcher := filepath.Join(directory, "runtime-command")
	writeRuntimeTestFile(t, dispatcher, []byte(`#!/bin/bash
set -eu
name=${0##*/}
printf '%s %s\n' "$name" "$*" >> "$TEST_CALLS"
case "$name" in
  uname) printf '6.8.0-142-generic\n' ;;
  dpkg-query)
    if [[ ${*: -1} == 'linux-image-virtual*' ]]; then
      [[ -z $TEST_VIRTUAL ]] || printf '%s\n' "$TEST_VIRTUAL"
    else
      [[ -n $TEST_EXTRA ]] || exit 1
      printf '%s\n' "$TEST_EXTRA"
    fi ;;
  modinfo) [[ $TEST_MODULE == ready ]] ;;
  apt-get)
    [[ $TEST_FAILURE != apt-$1 ]] || exit 1
    if [[ $1 == install ]]; then
      /bin/ln -sf "$0" "$TEST_BIN/modprobe"
    fi ;;
  modprobe) [[ $TEST_FAILURE != modprobe ]] ;;
  install)
    [[ $TEST_FAILURE != install ]] || exit 1
    if [[ ${*: -1} == /etc/modules-load.d/netlab-network.conf ]]; then
      /bin/cat > "$TEST_MODULE_FILE"
    fi ;;
esac
`), 0700)
	for _, name := range []string{"uname", "dpkg-query", "modinfo", "apt-get", "modprobe", "install", "suricata", "suricata-update", "systemctl", "logrotate"} {
		if missingKmod && (name == "modprobe" || name == "modinfo") {
			continue
		}
		if err := os.Symlink(dispatcher, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+":/usr/bin:/bin")
	t.Setenv("TEST_BIN", directory)
	t.Setenv("TEST_CALLS", log)
	t.Setenv("TEST_MODULE_FILE", module)
	for _, key := range []string{"TEST_VIRTUAL", "TEST_EXTRA", "TEST_MODULE", "TEST_FAILURE"} {
		t.Setenv(key, "")
	}
	return log, module
}

func writeRuntimeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
