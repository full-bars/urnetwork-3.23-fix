//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	shmLogPath    = "/dev/shm/urnetwork.log"
	shmLogMaxSize = 5 * 1024 * 1024 // 5MB target cap
	// keep the newest (trimRatio-1)/trimRatio, discard the oldest 1/trimRatio
	shmLogTrimRatio = 3

	// Second buffer holding ONLY high-value lines (see isImportantLogLine) so
	// the earnings/health signal survives even when the main 5MB buffer floods
	// in ~84s. It lives on disk (shmImportantLogDiskName in the state dir) so it
	// survives restarts and reboots; the lines are rare, so the write rate is
	// small. Surfaced via `urnet-tools logs --important`. A box with no usable
	// home directory falls back to the old RAM file.
	shmImportantLogPath       = "/dev/shm/urnetwork-important.log"
	shmImportantLogDiskName   = "important.log"
	shmImportantLogMaxSize    = 10 * 1024 * 1024 // per file; rotated once to .1, so 20MB on disk
	shmImportantLogMaxSizeRAM = 1 * 1024 * 1024
)

// ramlogsDockerEnvPath is the marker file that identifies a Docker container
// (/.dockerenv). A var so tests can point it at a temp file and exercise the
// container-ID fallback without root. TEST-ONLY mutation point: not
// concurrency-safe (tests restore it via defer; keep t.Parallel out of the
// shmlog tests).
var ramlogsDockerEnvPath = "/.dockerenv"

// ramlogOrigStdout and ramlogOrigStderr are close-on-exec copies of the
// descriptors stdout and stderr pointed at before initSHMLogger replaced them
// with the ramlog pipe; -1 until then. restoreStdioBeforeExec hands them back.
var ramlogOrigStdout, ramlogOrigStderr = -1, -1

// dupCloseOnExec duplicates fd with FD_CLOEXEC set atomically, so the copy
// never leaks into a child process.
func dupCloseOnExec(fd int) (int, error) {
	return unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
}

// restoreFD points target at the file behind saved. dup2 leaves target
// without FD_CLOEXEC, so it survives an execve.
func restoreFD(saved, target int) {
	if saved >= 0 {
		_ = dup2(saved, target)
	}
}

// restoreStdioBeforeExec points stdout and stderr back at what they were
// before the ramlog redirect, for an in-place execve (Docker HotSwap). The
// ramlog pipe is drained by a goroutine in this process, which exec throws
// away; left in place, the new image's first write dies of SIGPIPE before it
// logs anything, and the container's start script restarts it after its crash
// backoff. The new image sets up its own ramlog pipe as usual.
func restoreStdioBeforeExec() {
	restoreFD(ramlogOrigStdout, int(os.Stdout.Fd()))
	restoreFD(ramlogOrigStderr, int(os.Stderr.Fd()))
}

// shmInitOnce ensures initSHMLogger's banners, dup2, and goroutines are
// set up exactly once, even when called from multiple code paths (e.g.
// initGlog + initSHMLoggerWithHandover).
var shmInitOnce sync.Once

// ramlogsTailHint returns a copy-pasteable command to follow the shm log:
//   - `URNETWORK_CONTAINER_NAME` (deployment-pinned, e.g. "urfix") when set;
//   - the container ID (hostname) when running inside Docker — `docker exec`
//     accepts the container ID, so the hint works without knowing the --name;
//   - a plain `tail -F` on bare metal, where a docker hint would be wrong.
func ramlogsTailHint() string {
	if name := strings.TrimSpace(os.Getenv("URNETWORK_CONTAINER_NAME")); name != "" {
		return fmt.Sprintf("docker exec %s tail -F %s", name, shmLogPath)
	}
	if _, err := os.Stat(ramlogsDockerEnvPath); err == nil {
		if host, err := os.Hostname(); err == nil && host != "" {
			return fmt.Sprintf("docker exec %s tail -F %s", host, shmLogPath)
		}
	}
	return fmt.Sprintf("tail -F %s", shmLogPath)
}

// ramlogsTailHintWithTemplate returns the resolved hint; when the context is
// docker it is followed by the universal `<container>` template, so an
// operator whose container is named differently still has the form to copy
// (the resolved line is the primary copy-paste command).
func ramlogsTailHintWithTemplate() string {
	resolved := ramlogsTailHint()
	if strings.HasPrefix(resolved, "docker exec ") {
		return fmt.Sprintf(
			"%s\n[ramlogs] (any container name: docker exec <container> tail -F %s)",
			resolved, shmLogPath,
		)
	}
	return resolved
}

func initSHMLogger() {
	shmInitOnce.Do(func() {
		// O_APPEND preserves log across restarts so post-mortem analysis is
		// possible. O_RDWR rather than O_WRONLY because the trimmer below has
		// to read the newest portion back out before truncating; on a
		// write-only descriptor that read fails with EBADF and the trim
		// destroys the history it is supposed to keep.
		f, err := os.OpenFile(shmLogPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open shm log: %v\n", err)
			return
		}
		f.Write([]byte(fmt.Sprintf("\n--- provider restarted at %s ---\n", time.Now().Format(time.RFC3339))))
		f.Write([]byte(fmt.Sprintf("[ramlogs] Active at %s\n", shmLogPath)))
		f.Write([]byte(fmt.Sprintf("[ramlogs] View: %s\n", ramlogsTailHintWithTemplate())))

		// Print the same notice to the pre-redirect stdout/stderr, before the
		// dup2 below severs them from wherever they currently flow. For the
		// long-running `provide` process that's docker logs / journald; for a
		// one-shot CLI subcommand (e.g. `docker exec <container> ... proxy
		// remove-dead --preview`) it's the caller's own terminal. Without this,
		// URNETWORK_RAMLOGS=1 makes every invocation of the binary — not just
		// `provide` — go completely silent from the caller's point of view,
		// since the redirect applies process-wide, not just to the long-running
		// command.
		fmt.Fprintf(os.Stdout, "[ramlogs] output redirected to %s\n", shmLogPath)
		fmt.Fprintf(os.Stdout, "[ramlogs] view with: %s\n", ramlogsTailHintWithTemplate())

		r, w, err := os.Pipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create pipe: %v\n", err)
			return
		}

		// Keep the original stdout/stderr for restoreStdioBeforeExec.
		if fd, err := dupCloseOnExec(int(os.Stdout.Fd())); err == nil {
			ramlogOrigStdout = fd
		}
		if fd, err := dupCloseOnExec(int(os.Stderr.Fd())); err == nil {
			ramlogOrigStderr = fd
		}

		dup2(int(w.Fd()), int(os.Stdout.Fd()))
		dup2(int(w.Fd()), int(os.Stderr.Fd()))

		// fMu guards the descriptor the reader writes to, and nothing else.
		// A trim renames a new file over shmLogPath, so the reader must not
		// cache `f`: it writes through logFile, which the trimmer swaps under
		// this lock. Holding the lock only for a write or a swap is the point
		// of the rename trim - the copy of the kept tail happens outside it,
		// so the 64 KiB pipe keeps draining while a trim runs.
		var fMu sync.Mutex
		logFile := f

		// Second buffer: high-value lines only. Best-effort; nil disables mirroring.
		// O_RDWR for the same reason as the main log: the trimmer reads before
		// it truncates.
		impPath, impMax := importantLogLocation()
		fImp, impErr := os.OpenFile(impPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
		if impErr != nil && impPath != shmImportantLogPath {
			// the state dir is not writable: fall back to the RAM file
			impPath, impMax = shmImportantLogPath, int64(shmImportantLogMaxSizeRAM)
			fImp, impErr = os.OpenFile(impPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
		}
		if impErr != nil {
			fmt.Fprintf(os.Stderr, "failed to open important log: %v\n", impErr)
			fImp = nil
		} else if impPath != shmImportantLogPath {
			// the RAM file from before the move is never written again; leaving it
			// would show operators stale data
			os.Remove(shmImportantLogPath)
		}
		impEnabled := fImp != nil // fImp itself is swapped under fImpMu when it rotates
		var fImpMu sync.Mutex

		go func() {
			defer fMu.Lock()
			defer f.Close()
			defer fMu.Unlock()
			defer r.Close()
			defer w.Close()

			buf := make([]byte, 32*1024)
			var lineBuf []byte // accumulates a partial trailing line across reads

			for {
				n, err := r.Read(buf)
				if n > 0 {
					fMu.Lock()
					logFile.Write(buf[:n])
					fMu.Unlock()

					// Mirror complete important lines into the small buffer. Both
					// tlog and glog output flow through this pipe, so this captures
					// [profit]/[earn]/[health]/etc. regardless of which logger emit.
					if impEnabled {
						lineBuf = append(lineBuf, buf[:n]...)
						for {
							i := bytes.IndexByte(lineBuf, '\n')
							if i < 0 {
								break
							}
							line := lineBuf[:i+1] // include newline
							if isImportantLogLine(string(line)) {
								fImpMu.Lock()
								fImp.Write(line)
								fImpMu.Unlock()
							}
							lineBuf = lineBuf[i+1:]
						}
						if len(lineBuf) > 1<<20 { // guard a pathological no-newline stream
							lineBuf = lineBuf[:0]
						}
					}
				}
				if err != nil {
					break
				}
			}
		}()

		// Keep the important buffer bounded. On disk it rotates by rename (one
		// previous file, no in-place rewrite, no periodic fsync: the lines are
		// rare and a crash may lose the last few seconds). The RAM fallback is
		// trimmed in place like the main log.
		if impEnabled {
			go func() {
				for {
					time.Sleep(5 * time.Second)
					fImpMu.Lock()
					if impPath == shmImportantLogPath {
						err := trimRAMLog(fImp, impMax, shmLogTrimRatio)
						if err == nil {
							fImp.Sync()
						}
						if err != nil {
							fImpMu.Unlock()
							reportTrimFailure(&impTrimWarned, &impTrimWarnTime, impPath, err)
							continue
						}
					} else if err := rotateImportantLog(&fImp, impPath, impMax); err != nil {
						fImpMu.Unlock()
						reportTrimFailure(&impTrimWarned, &impTrimWarnTime, impPath, err)
						continue
					}
					fImpMu.Unlock()
				}
			}()
		}

		// Trim the file down when it exceeds shmLogMaxSize, keeping the newest
		// portion and discarding the oldest. This is a ring-buffer-lite: the
		// file stays near 5MB, the most recent content is always preserved, and
		// the tail loop in urnet-tools logs (which reopens on fi.Size() < pos)
		// handles the size change transparently.
		go func() {
			// The rename can succeed and the reopen still fail, for example with
			// EMFILE on a process holding many sockets. At that point the old
			// inode is already unlinked: the reader would keep writing to a file
			// no path refers to, nothing would trim it, and no later pass would
			// retry the swap because the replacement file is small. So the swap
			// stays pending and the open is retried every pass until it works.
			pendingSwap := false
			for {
				time.Sleep(5 * time.Second)
				if !pendingSwap {
					trimmed, err := trimRAMLogByRename(shmLogPath, shmLogMaxSize, shmLogTrimRatio)
					if err != nil {
						reportTrimFailure(&mainTrimWarned, &mainTrimWarnTime, shmLogPath, err)
						continue
					}
					if !trimmed {
						continue
					}
					pendingSwap = true
				}
				next, err := os.OpenFile(shmLogPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
				if err != nil {
					reportTrimFailure(&mainTrimWarned, &mainTrimWarnTime, shmLogPath, err)
					continue
				}
				fMu.Lock()
				previous := logFile
				logFile = next
				fMu.Unlock()
				previous.Close()
				pendingSwap = false
			}
		}()

		// Periodically sync to ensure tail -F sees updates quickly
		go func() {
			for {
				time.Sleep(500 * time.Millisecond)
				// An fsync on an append-only descriptor is safe beside a
				// concurrent write, so the sync reads the current descriptor
				// under the lock and then flushes outside it. Holding the lock
				// across the fsync was the second half of the convoy: on a busy
				// /dev/shm it stalls the pipe for as long as the kernel takes.
				fMu.Lock()
				current := logFile
				fMu.Unlock()
				current.Sync()
			}
		}()
	})
}

func shmLogFatal(code int, format string, args ...any) {
	msg := fmt.Sprintf("FATAL [exit %d]: %s\n", code, fmt.Sprintf(format, args...))
	if f, err := os.OpenFile(shmLogPath, os.O_WRONLY|os.O_APPEND, 0); err == nil {
		f.Write([]byte(msg))
		f.Sync()
		f.Close()
	}
	os.Stderr.Write([]byte(msg))
	critLog("FATAL exit=%d: %s", code, fmt.Sprintf(format, args...))
	os.Exit(code)
}

// importantLogLocation is where the important buffer lives and how big it may
// grow: the state dir on disk, or the RAM file when there is no state dir.
func importantLogLocation() (string, int64) {
	if dir := mustStateDir(); dir != "" {
		if err := os.MkdirAll(dir, 0700); err == nil {
			return filepath.Join(dir, shmImportantLogDiskName), shmImportantLogMaxSize
		}
	}
	return shmImportantLogPath, shmImportantLogMaxSizeRAM
}

// rotateImportantLog moves the on-disk important log to path+".1" once it
// passes max and reopens it, so a previous file is always kept and nothing is
// rewritten in place. The caller holds the buffer's mutex.
func rotateImportantLog(f **os.File, path string, max int64) error {
	info, err := (*f).Stat()
	if err != nil || info.Size() < max {
		return err
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	next, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	(*f).Close()
	*f = next
	return nil
}
