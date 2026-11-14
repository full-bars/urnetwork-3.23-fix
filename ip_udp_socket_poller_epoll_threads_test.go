//go:build linux

package connect

import (
	"bufio"
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// processThreadCount reads the OS thread count of this process.
func processThreadCount(t *testing.T) int {
	t.Helper()
	file, err := os.Open("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status: %v", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Threads:") {
			count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Threads:")))
			if err != nil {
				t.Fatal(err)
			}
			return count
		}
	}
	t.Fatal("no Threads line in /proc/self/status")
	return 0
}

func udpSocketFd(t *testing.T, conn *net.UDPConn) int {
	t.Helper()
	rawConn, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	fd := -1
	if err := rawConn.Control(func(rawFd uintptr) { fd = int(rawFd) }); err != nil {
		t.Fatal(err)
	}
	return fd
}

// A readiness shard that waits for sockets must not hold an OS thread. The
// provider runs a few shards per proxy, so one thread per shard becomes
// thousands of threads on a host with a thousand proxies.
func TestUdpSocketEpollBackendWaitDoesNotPinThreads(t *testing.T) {
	const shardCount = 300
	before := processThreadCount(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backends := make([]udpSocketPollBackend, 0, shardCount)
	for range shardCount {
		backend, err := newUdpSocketPollBackend()
		if err != nil {
			t.Fatal(err)
		}
		backends = append(backends, backend)
	}

	var entered sync.WaitGroup
	var done sync.WaitGroup
	for _, backend := range backends {
		entered.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			events := make([]udpSocketPollEvent, udpSocketPollEventCount)
			entered.Done()
			// the shard loop: wait, observe cancellation, wait again
			for ctx.Err() == nil {
				if _, err := backend.wait(events); err != nil {
					return
				}
			}
		}()
	}
	entered.Wait()

	// Let every goroutine settle into its waits. The check polls until the
	// thread count stops changing or a generous bound passes.
	deadline := time.Now().Add(5 * time.Second)
	settled := processThreadCount(t)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		now := processThreadCount(t)
		if now == settled {
			break
		}
		settled = now
	}

	// Before the netpoller wait this grew by about shardCount.
	if growth := settled - before; 40 < growth {
		t.Fatalf("%d waiting shards grew the process by %d threads (before %d, after %d); a shard must not pin an OS thread", shardCount, growth, before, settled)
	}

	cancel()
	done.Wait()
	for _, backend := range backends {
		if err := backend.close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUdpSocketEpollBackendReadinessTimeoutAndClose(t *testing.T) {
	backend, err := newUdpSocketPollBackend()
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.DialUDP("udp4", nil, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	fd := udpSocketFd(t, receiver)
	if err := backend.add(fd); err != nil {
		t.Fatal(err)
	}
	events := make([]udpSocketPollEvent, udpSocketPollEventCount)

	// timeout: nothing is ready, the wait reports zero events and no error
	n, err := backend.wait(events)
	if err != nil || n != 0 {
		t.Fatalf("idle wait = %d, %v; want a quiet timeout", n, err)
	}

	// readiness: a datagram that arrives while the shard is parked ends the wait
	type waitResult struct {
		n      int
		events []udpSocketPollEvent
		err    error
	}
	waitOnce := func() chan waitResult {
		out := make(chan waitResult, 1)
		go func() {
			local := make([]udpSocketPollEvent, udpSocketPollEventCount)
			for {
				n, err := backend.wait(local)
				if err != nil || 0 < n {
					out <- waitResult{n: n, events: local[:max(n, 0)], err: err}
					return
				}
			}
		}()
		return out
	}
	pending := waitOnce()
	if _, err := sender.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-pending:
		if result.err != nil || result.n != 1 || result.events[0].fd != fd || result.events[0].terminal {
			t.Fatalf("readiness wait = %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a ready socket did not end the wait")
	}
	buffer := make([]byte, 16)
	if _, _, err := receiver.ReadFromUDP(buffer); err != nil {
		t.Fatal(err)
	}

	// remove: the socket no longer produces events
	backend.remove(fd)
	if _, err := sender.Write([]byte("after remove")); err != nil {
		t.Fatal(err)
	}
	if n, err := backend.wait(events); err != nil || n != 0 {
		t.Fatalf("wait after remove = %d, %v; want a quiet timeout", n, err)
	}

	// close: ends a parked wait with an error and does not hang
	done := make(chan error, 1)
	go func() {
		local := make([]udpSocketPollEvent, udpSocketPollEventCount)
		for {
			if _, err := backend.wait(local); err != nil {
				done <- err
				return
			}
		}
	}()
	if err := backend.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a closed backend wait returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not end a waiting shard")
	}
	if err := backend.close(); err != nil {
		t.Fatalf("a second close = %v", err)
	}
}
