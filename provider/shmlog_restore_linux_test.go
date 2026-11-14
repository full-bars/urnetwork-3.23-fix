//go:build linux

package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestRestoreFDRedirectsWrites: after restoreFD, writes to the target land in
// the saved file, the way stdout returns from the ramlog pipe to its original
// destination before an execve.
func TestRestoreFDRedirectsWrites(t *testing.T) {
	orig, err := os.Create(filepath.Join(t.TempDir(), "orig"))
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	saved, err := dupCloseOnExec(int(orig.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(saved)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	target, err := unix.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(target)

	restoreFD(saved, target)
	if _, err := unix.Write(target, []byte("after restore")); err != nil {
		t.Fatalf("write after restore: %v", err)
	}
	got, err := os.ReadFile(orig.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after restore" {
		t.Fatalf("restored target wrote %q to the original file, want %q", got, "after restore")
	}
	flags, err := unix.FcntlInt(uintptr(target), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("restored target is close-on-exec; it would not survive execve")
	}
}

// TestDupCloseOnExecSetsFlag: the saved copies must not leak into children.
func TestDupCloseOnExecSetsFlag(t *testing.T) {
	fd, err := dupCloseOnExec(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("saved descriptor is not close-on-exec")
	}
}

// TestRestoreFDIgnoresUnsaved: nothing to restore when the redirect never ran.
func TestRestoreFDIgnoresUnsaved(t *testing.T) {
	restoreFD(-1, 1)
}

func TestHotSwapStderrHelper(t *testing.T) {
	mode := os.Getenv("TEST_HOTSWAP_STDERR")
	if mode == "" {
		return
	}
	if mode == "old" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGPIPE)
		signal.Stop(ch)
	}
	if mode == "live-successor" {
		if _, err := log.Writer().Write([]byte("live stderr\n")); err != nil {
			os.Exit(7)
		}
		var b [1]byte
		if _, err := syscall.Read(3, b[:]); err != nil {
			os.Exit(8)
		}
		if _, err := log.Writer().Write(make([]byte, 256*1024)); err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	}
	if mode == "successor" {
		if _, err := log.Writer().Write(make([]byte, 256*1024)); err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	}
	if mode == "successor-stdout" {
		if _, err := os.Stdout.Write([]byte("candidate stdout\n")); err != nil {
			os.Exit(4)
		}
		os.Exit(0)
	}
	if mode == "spawned" {
		var b [1]byte
		if _, err := syscall.Read(3, b[:]); err != nil {
			os.Exit(2)
		}
	}
	_, err := log.Writer().Write([]byte("candidate stderr\n"))
	if mode == "ignored" && errors.Is(err, syscall.EPIPE) {
		os.Exit(0)
	}
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestHotSwapOrphanedStderr(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"old", "ignored", "successor"} {
		t.Run(mode, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			defer w.Close()
			cmd := exec.Command(exe, "-test.run=^TestHotSwapStderrHelper$")
			cmd.Stdout, cmd.Stderr = w, w
			cmd.Env = append(os.Environ(), "TEST_HOTSWAP_STDERR="+mode)
			if mode == "successor" {
				cmd.Env = append(cmd.Env, EnvHotSwap+"=1")
			}
			err = cmd.Run()
			if mode == "old" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGPIPE {
					t.Fatalf("old behavior: expected SIGPIPE, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("successor did not survive: %v", err)
			}
		})
	}
}

func TestHotSwapNormalStderrUnchanged(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHotSwapStderrHelper$")
	cmd.Stdout, cmd.Stderr = w, w
	cmd.Env = append(os.Environ(), "TEST_HOTSWAP_STDERR=normal", EnvHotSwap+"=0")
	err = cmd.Run()
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "candidate stderr\n" {
		t.Fatalf("normal stderr = %q, %v", got, err)
	}
}

func TestHotSwapSuccessorPreservesStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHotSwapStderrHelper$")
	cmd.Stdout, cmd.Stderr = w, w
	cmd.Env = append(os.Environ(), "TEST_HOTSWAP_STDERR=successor-stdout", EnvHotSwap+"=1")
	err = cmd.Run()
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "candidate stdout\n" {
		t.Fatalf("successor stdout = %q, %v", got, err)
	}
}

func TestHotSwapForwardsLiveStderrAndDrainsAfterClose(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fds, err := hotSwapSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(fds[0]), "parent")
	child := os.NewFile(uintptr(fds[1]), "child")
	defer parent.Close()
	exe, err := os.Executable()
	if err != nil {
		child.Close()
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHotSwapStderrHelper$")
	cmd.Stdout = io.Discard
	cmd.Stderr = w
	cmd.ExtraFiles = []*os.File{child}
	cmd.Env = append(os.Environ(), "TEST_HOTSWAP_STDERR=live-successor", EnvHotSwap+"=1")
	if err := cmd.Start(); err != nil {
		child.Close()
		t.Fatal(err)
	}
	child.Close()
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("live stderr not forwarded: %v", err)
		}
		if line == "live stderr\n" {
			break
		}
	}
	r.Close()
	if _, err := parent.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("successor after reader close: %v", err)
	}
}

func TestHotSwapCandidateUsesOriginalStderr(t *testing.T) {
	orig, err := os.Create(filepath.Join(t.TempDir(), "original-stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	origOut, err := os.Create(filepath.Join(t.TempDir(), "original-stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer origOut.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	oldSavedOut, oldSavedErr := ramlogOrigStdout, ramlogOrigStderr
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		ramlogOrigStdout, ramlogOrigStderr = oldSavedOut, oldSavedErr
	}()
	os.Stdout, os.Stderr = w, w
	ramlogOrigStdout, ramlogOrigStderr = int(origOut.Fd()), int(orig.Fd())
	t.Setenv("TEST_HOTSWAP_STDERR", "spawned")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	session, err := spawnHotSwapCandidate(exe, []string{"-test.run=^TestHotSwapStderrHelper$"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	r.Close()
	if _, err := session.parentFd.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("candidate: %v", err)
	}
	contents, err := os.ReadFile(orig.Name())
	if err != nil || string(contents) != "candidate stderr\n" {
		t.Fatalf("original stderr = %q, %v", contents, err)
	}
}
