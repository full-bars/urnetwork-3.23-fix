//go:build linux || android

package connect

import (
	"sync"

	"golang.org/x/sys/unix"
)

// udpSocketEpollBackend is the linux/android readiness backend. The descriptor
// is level-triggered, so a newly registered socket with pending datagrams is
// reported by the wait already in progress without an explicit wake.
type udpSocketEpollBackend struct {
	mutex    sync.Mutex
	fd       int
	osEvents [udpSocketPollEventCount]unix.EpollEvent
}

func newUdpSocketPollBackend() (udpSocketPollBackend, error) {
	fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return &udpSocketEpollBackend{fd: fd}, nil
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
	n, err := unix.EpollWait(
		self.fd,
		self.osEvents[:min(len(events), len(self.osEvents))],
		udpSocketPollTimeoutMillis,
	)
	for i := range n {
		event := self.osEvents[i]
		events[i] = udpSocketPollEvent{
			fd:       int(event.Fd),
			terminal: event.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0,
		}
	}
	return n, err
}

func (self *udpSocketEpollBackend) close() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.fd < 0 {
		return nil
	}
	err := unix.Close(self.fd)
	self.fd = -1
	return err
}
