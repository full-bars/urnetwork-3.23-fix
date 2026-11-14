//go:build linux || android

package connect

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// udpSocketEpollBackend is the linux/android readiness backend. The descriptor
// is level-triggered, so a newly registered socket with pending datagrams is
// reported by the wait already in progress without an explicit wake.
//
// The wait goes through the Go runtime's network poller. The epoll descriptor
// is nonblocking and wrapped in an os.File, and the poll timeout is a read
// deadline, so a shard with nothing ready parks as a goroutine. A blocking
// epoll_wait would hold one OS thread per shard for the life of the process,
// and a provider runs a few shards for every proxy.
type udpSocketEpollBackend struct {
	mutex    sync.Mutex
	fd       int
	file     *os.File
	rawConn  syscall.RawConn
	osEvents [udpSocketPollEventCount]unix.EpollEvent
}

func newUdpSocketPollBackend() (udpSocketPollBackend, error) {
	fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	// From here the file owns fd: closing the file closes the descriptor.
	file := os.NewFile(uintptr(fd), "udp-socket-epoll")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("udp socket epoll: cannot wrap the epoll descriptor")
	}
	rawConn, err := file.SyscallConn()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &udpSocketEpollBackend{fd: fd, file: file, rawConn: rawConn}, nil
}

func (self *udpSocketEpollBackend) add(fd int) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.fd < 0 {
		return unix.EBADF
	}
	return unix.EpollCtl(self.fd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLERR | unix.EPOLLHUP,
		Fd:     int32(fd),
	})
}

func (self *udpSocketEpollBackend) remove(fd int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.fd < 0 {
		return
	}
	_ = unix.EpollCtl(self.fd, unix.EPOLL_CTL_DEL, fd, nil)
}

func (self *udpSocketEpollBackend) wait(events []udpSocketPollEvent) (int, error) {
	// The callback polls without blocking. When nothing is ready the runtime
	// parks this goroutine until the epoll descriptor becomes readable or the
	// deadline passes, so the wait costs no OS thread. A timeout reports zero
	// events, as the blocking wait did.
	limit := self.osEvents[:min(len(events), len(self.osEvents))]
	_ = self.file.SetReadDeadline(time.Now().Add(time.Duration(udpSocketPollTimeoutMillis) * time.Millisecond))
	var n int
	var waitErr error
	readErr := self.rawConn.Read(func(fd uintptr) bool {
		n, waitErr = unix.EpollWait(int(fd), limit, 0)
		if waitErr != nil {
			return !errors.Is(waitErr, syscall.EINTR)
		}
		return 0 < n
	})
	if readErr != nil {
		if errors.Is(readErr, os.ErrDeadlineExceeded) {
			return 0, nil
		}
		return 0, readErr
	}
	if waitErr != nil {
		return 0, waitErr
	}
	for i := range n {
		event := self.osEvents[i]
		events[i] = udpSocketPollEvent{
			fd:       int(event.Fd),
			terminal: event.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0,
		}
	}
	return n, nil
}

func (self *udpSocketEpollBackend) close() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.fd < 0 {
		return nil
	}
	// Closing the file closes the epoll descriptor and ends a parked wait.
	err := self.file.Close()
	self.fd = -1
	return err
}
