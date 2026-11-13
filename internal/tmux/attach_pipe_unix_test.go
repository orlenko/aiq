//go:build unix

package tmux

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Transfer real command output pipes to the test owner, which retains them
// after this helper exits. This models inherited writers without orphaning a
// descendant or changing process-wide child-reaping behavior.
func TestAttachRetainedPipeChild(t *testing.T) {
	if os.Getenv("GO_ATTACH_PIPE_CHILD") != "1" {
		return
	}
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: os.Getenv("GO_ATTACH_PIPE_SOCKET"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.WriteMsgUnix([]byte{1}, unix.UnixRights(1, 2), nil); err != nil {
		t.Fatal(err)
	}
	// TestMain prints PASS after this valid control record. The pipe-drain
	// refusal must happen before metadata parsing; malformed output cannot
	// substitute for the bounded-return assertion in the parent.
	_, _ = os.Stdout.WriteString("%17|2468|0|$3|@9|/fixture/socket\n")
}

func TestAttachRetainedPipesBoundedRefusal(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "aiq-pipe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "transfer")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	type received struct {
		files []*os.File
		err   error
	}
	transferred := make(chan received, 1)
	var receiver, preparation sync.WaitGroup
	var retained []*os.File
	// Install cleanup before starting either goroutine. Even the no-WaitDelay
	// fault check must release EOF and join the actual command/copy workers.
	t.Cleanup(func() {
		listener.Close()
		receiver.Wait()
		select {
		case got := <-transferred:
			retained = append(retained, got.files...)
		default:
		}
		for _, file := range retained {
			file.Close()
		}
		preparation.Wait()
	})
	receiver.Add(1)
	go func() {
		defer receiver.Done()
		got := received{}
		defer func() { transferred <- got }()
		c, err := listener.AcceptUnix()
		if err != nil {
			got.err = err
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		data, control := make([]byte, 1), make([]byte, unix.CmsgSpace(2*4))
		n, oob, flags, _, err := c.ReadMsgUnix(data, control)
		if err != nil {
			got.err = err
			return
		}
		messages, err := unix.ParseSocketControlMessage(control[:oob])
		if err != nil {
			got.err = err
			return
		}
		for _, message := range messages {
			fds, err := unix.ParseUnixRights(&message)
			if err != nil {
				got.err = err
				continue
			}
			for _, fd := range fds {
				unix.CloseOnExec(fd)
				got.files = append(got.files, os.NewFile(uintptr(fd), "retained-output"))
			}
		}
		if n != 1 || data[0] != 1 || flags&unix.MSG_CTRUNC != 0 || len(got.files) != 2 {
			got.err = ErrAttachUnavailable
		}
	}()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "tmux")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec \"$GO_ATTACH_PIPE_SELF\" -test.run=^TestAttachRetainedPipeChild$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_ATTACH_PIPE_CHILD", "1")
	t.Setenv("GO_ATTACH_PIPE_SOCKET", socket)
	t.Setenv("GO_ATTACH_PIPE_SELF", self)
	// Race instrumentation ordinarily sleeps for a second at successful exit;
	// keep that artificial delay out of this child-exit/pipe-drain oracle.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv("TMUX", "")
	result := make(chan error, 1)
	preparation.Add(1)
	go func() {
		defer preparation.Done()
		action, err := prepareAttach("%17", 2468, os.Environ(), binary, 500*time.Millisecond)
		if action != nil || err != ErrAttachUnavailable {
			result <- ErrAttachChanged
			return
		}
		result <- nil
	}()
	select {
	case got := <-transferred:
		retained = got.files
		if got.err != nil || len(retained) != 2 {
			t.Fatal("output-pipe transfer failed", got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("output-pipe transfer did not complete")
	}
	// Both writers remain owned and open until cleanup, so EOF has not been
	// manufactured by the test to let the control command return.
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("retained pipes did not produce fixed refusal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata waited for retained output pipes")
	}
}
