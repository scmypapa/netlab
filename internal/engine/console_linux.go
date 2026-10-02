//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/namespaces"
	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

type Console struct {
	io.ReadWriteCloser
	Resize func(context.Context, uint32, uint32) error
}

func (e *Engine) OpenConsole(ctx context.Context, env, asset, instance string, kind api.ConsoleKind) (*Console, error) {
	switch kind {
	case "terminal":
		if e.container == nil {
			return nil, errors.New("container runtime is not configured")
		}
		return e.container.console(ctx, env, asset, instance)
	case "vnc", "serial":
		if e.vm == nil {
			return nil, errors.New("virtual machine runtime is not configured")
		}
		return e.vm.console(ctx, env, asset, instance, kind)
	default:
		return nil, errors.New("invalid console kind")
	}
}

type terminal struct {
	reader  *io.PipeReader
	writer  *io.PipeWriter
	input   *io.PipeReader
	output  *io.PipeWriter
	process containerd.Process
	ctx     context.Context
	once    sync.Once
	err     error
}

func (t *terminal) Read(p []byte) (int, error)  { return t.reader.Read(p) }
func (t *terminal) Write(p []byte) (int, error) { return t.writer.Write(p) }
func (t *terminal) Close() error {
	t.once.Do(func() {
		t.input.Close()
		t.writer.Close()
		t.reader.Close()
		t.output.Close()
		_, t.err = t.process.Delete(context.WithoutCancel(t.ctx), containerd.WithProcessKill)
	})
	return t.err
}

func (c *Containers) console(ctx context.Context, env, asset, instance string) (*Console, error) {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	container, err := c.client.LoadContainer(ctx, instance)
	if err != nil {
		return nil, err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return nil, err
	}
	if labels[environmentLabel] != env || labels[assetLabel] != asset {
		return nil, errors.New("container ownership does not match console")
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return nil, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return nil, err
	}
	processSpec := *spec.Process
	processSpec.Args = []string{"/bin/sh"}
	processSpec.Cwd = "/"
	processSpec.Terminal = true
	processSpec.Env = append(processSpec.Env, "TERM=xterm-256color")
	in, stdin := io.Pipe()
	stdout, out := io.Pipe()
	process, err := task.Exec(ctx, uuid.NewString(), &processSpec, cio.NewCreator(cio.WithTerminal, cio.WithStreams(in, out, nil)))
	if err != nil {
		in.Close()
		stdin.Close()
		stdout.Close()
		out.Close()
		return nil, err
	}
	t := &terminal{reader: stdout, writer: stdin, input: in, output: out, process: process, ctx: ctx}
	exited, err := process.Wait(ctx)
	if err == nil {
		err = process.Start(ctx)
	}
	if err != nil {
		return nil, errors.Join(err, t.Close())
	}
	go func() {
		<-exited
		in.Close()
		process.IO().Wait()
		out.Close()
	}()
	return &Console{ReadWriteCloser: t, Resize: process.Resize}, nil
}

type serial struct {
	stream *libvirt.Stream
	domain *libvirt.Domain
	once   sync.Once
	err    error
}

func (s *serial) Read(p []byte) (int, error) {
	n, err := s.stream.Recv(p)
	if n == 0 && err == nil {
		err = io.EOF
	}
	return n, err
}
func (s *serial) Write(p []byte) (int, error) { return s.stream.Send(p) }
func (s *serial) Close() error {
	s.once.Do(func() { s.err = errors.Join(s.stream.Abort(), s.stream.Free(), s.domain.Free()) })
	return s.err
}

func (v *VirtualMachines) console(ctx context.Context, env, asset, instance string, kind api.ConsoleKind) (*Console, error) {
	domain, err := v.conn.LookupDomainByUUIDString(instance)
	if err != nil {
		return nil, err
	}
	if _, err = v.owned(domain, env, asset); err != nil {
		domain.Free()
		return nil, err
	}
	if kind == "serial" {
		stream, err := v.conn.NewStream(0)
		if err != nil {
			domain.Free()
			return nil, err
		}
		if err = domain.OpenConsole("", stream, 0); err != nil {
			stream.Free()
			domain.Free()
			return nil, err
		}
		return &Console{ReadWriteCloser: &serial{stream: stream, domain: domain}}, nil
	}
	defer domain.Free()
	text, err := domain.GetXMLDesc(0)
	if err != nil {
		return nil, err
	}
	var config libvirtxml.Domain
	if err = config.Unmarshal(text); err != nil {
		return nil, err
	}
	for _, graphic := range config.Devices.Graphics {
		if graphic.VNC != nil && graphic.VNC.Port > 0 {
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(graphic.VNC.Port))
			connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
			if err != nil {
				return nil, err
			}
			return &Console{ReadWriteCloser: connection}, nil
		}
	}
	return nil, fmt.Errorf("VM %s has no active VNC console", asset)
}
