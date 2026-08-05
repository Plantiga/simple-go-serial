// This portion of the package has no automated tests; it is exercised
// against real hardware (Plantiga docks) on Windows.

package serial

import (
	"braces.dev/errtrace"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ERROR_OPERATION_ABORTED: what GetOverlappedResult reports for an operation
// that was cancelled with CancelIoEx.
const errOperationAborted syscall.Errno = 995

type Port struct {
	f          *os.File
	fd         syscall.Handle
	rl         sync.Mutex
	wl         sync.Mutex
	ro         *syscall.Overlapped
	wo         *syscall.Overlapped
	DeviceName string
	// Windows can only query the input modem lines (CTS/DSR/RING/DCD), so
	// the output lines are tracked as last-commanded state instead.
	dtr bool
	rts bool
	// guarded by rl+wl; makes Close idempotent so the event handles can't
	// be double-closed (a second CloseHandle could hit a recycled handle).
	closed bool
	// set by Close before it cancels in-flight I/O; Read/Write consult it so
	// an operation issued concurrently with Close still gets cancelled and
	// the I/O locks settle (reads block indefinitely waiting for data now
	// that the driver-side comm timeouts are gone).
	closing atomic.Bool
	// dlMu guards deadline. One deadline covers both reads and writes,
	// mirroring how the POSIX ports use os.File.SetDeadline.
	dlMu     sync.Mutex
	deadline time.Time
}

func (p *Port) Read(buf []byte) (int, error) {
	if p == nil || p.f == nil {
		return 0, errtrace.Wrap(fmt.Errorf("Invalid port on read %v %v", p, p.f))
	}

	p.rl.Lock()
	defer p.rl.Unlock()

	// After Close the fd and event handle values may have been recycled by
	// the OS; they must not reach any Windows API.
	if p.closed || p.closing.Load() {
		return 0, errtrace.Wrap(os.ErrClosed)
	}

	timeout, err := p.waitTimeout()
	if err != nil {
		return 0, errtrace.Wrap(err)
	}
	if err := resetEvent(p.ro.HEvent); err != nil {
		return 0, errtrace.Wrap(err)
	}
	var done uint32
	err = syscall.ReadFile(p.fd, buf, &done, p.ro)
	if err != nil && err != syscall.ERROR_IO_PENDING {
		return int(done), errtrace.Wrap(err)
	}
	return errtrace.Wrap2(p.waitOverlapped(p.ro, timeout))
}

func (p *Port) Write(buf []byte) (int, error) {
	p.wl.Lock()
	defer p.wl.Unlock()

	// After Close the fd and event handle values may have been recycled by
	// the OS; they must not reach any Windows API.
	if p.closed || p.closing.Load() {
		return 0, errtrace.Wrap(os.ErrClosed)
	}

	timeout, err := p.waitTimeout()
	if err != nil {
		return 0, errtrace.Wrap(err)
	}
	if err := resetEvent(p.wo.HEvent); err != nil {
		return 0, errtrace.Wrap(err)
	}
	var n uint32
	err = syscall.WriteFile(p.fd, buf, &n, p.wo)
	if err != nil && err != syscall.ERROR_IO_PENDING {
		return int(n), errtrace.Wrap(err)
	}
	written, err := p.waitOverlapped(p.wo, timeout)
	if err == nil && written < len(buf) {
		// With no driver-side write timeout, WriteFile only completes short
		// when a cancel reaped a partial transfer; io.Writer requires an
		// error alongside a short count.
		if p.closing.Load() {
			return written, errtrace.Wrap(os.ErrClosed)
		}
		return written, errtrace.Wrap(os.ErrDeadlineExceeded)
	}
	return written, errtrace.Wrap(err)
}

func (p *Port) Close() error {
	// Reads block until data arrives (there are no driver-side comm
	// timeouts), so any in-flight operation must be cancelled or the I/O
	// locks below would never settle. The order matters: the flag is set
	// first, so an operation issued after the CancelIoEx below sees it in
	// waitOverlapped and cancels itself.
	// Only the first Close may touch p.fd here: once it completes, the
	// handle value is closed and may have been recycled by the OS.
	if !p.closing.Swap(true) {
		// A nil overlapped cancels every pending operation on the handle
		// regardless of which thread issued it; ERROR_NOT_FOUND just means
		// nothing was in flight.
		syscall.CancelIoEx(p.fd, nil)
	}

	// Take both IO locks so the event handles aren't closed out from under
	// an in-flight overlapped operation.
	p.rl.Lock()
	defer p.rl.Unlock()
	p.wl.Lock()
	defer p.wl.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true

	err := p.f.Close()
	// The event handles created by newOverlapped are owned by the port and
	// are not released by closing the file.
	if closeErr := syscall.CloseHandle(p.ro.HEvent); err == nil {
		err = closeErr
	}
	if closeErr := syscall.CloseHandle(p.wo.HEvent); err == nil {
		err = closeErr
	}
	return errtrace.Wrap(err)
}

// comstat mirrors the Win32 COMSTAT struct. The first DWORD packs a set of
// status bit-fields (fCtsHold, fDsrHold, …); the fields we care about are the
// two queue depths that follow.
type comstat struct {
	flags    uint32
	cbInQue  uint32
	cbOutQue uint32
}

// InWaiting returns the number of bytes sitting in the driver's receive queue,
// matching the POSIX TIOCINQ semantics used on the other platforms. Without
// this the buffering read loop falls back to one-byte reads and cannot drain a
// fast stream before the driver's RX buffer overruns.
func (p *Port) InWaiting() (int, error) {
	var errs uint32
	var stat comstat
	r, _, err := syscall.Syscall(nClearCommError, 3,
		uintptr(p.fd),
		uintptr(unsafe.Pointer(&errs)),
		uintptr(unsafe.Pointer(&stat)))
	if r == 0 {
		return 0, errtrace.Wrap(err)
	}
	return int(stat.cbInQue), nil
}

// PurgeComm flags
const (
	purgeTxClear = 0x0004
	purgeRxClear = 0x0008
)

// EscapeCommFunction codes
const (
	escapeSetRTS = 3
	escapeClrRTS = 4
	escapeSetDTR = 5
	escapeClrDTR = 6
)

// GetCommModemStatus bits
const msRLSDOn = 0x0080 // DCD (receive-line-signal-detect)

// ResetInputBuffer discards data received but not yet read (tcflush TCIFLUSH).
func (p *Port) ResetInputBuffer() error {
	return errtrace.Wrap(purgeComm(p.fd, purgeRxClear))
}

// ResetOutputBuffer discards data written but not yet transmitted (tcflush TCOFLUSH).
func (p *Port) ResetOutputBuffer() error {
	return errtrace.Wrap(purgeComm(p.fd, purgeTxClear))
}

// SetDTR sets the status of the DTR line of a port to the given state,
// allowing manual control of the Data Terminal Ready modem line.
func (p *Port) SetDTR(state bool) error {
	code := escapeClrDTR
	if state {
		code = escapeSetDTR
	}
	if err := escapeCommFunction(p.fd, code); err != nil {
		return errtrace.Wrap(err)
	}
	p.dtr = state
	return nil
}

// SetRTS sets the status of the RTS line of a port to the given state.
func (p *Port) SetRTS(state bool) error {
	code := escapeClrRTS
	if state {
		code = escapeSetRTS
	}
	if err := escapeCommFunction(p.fd, code); err != nil {
		return errtrace.Wrap(err)
	}
	p.rts = state
	return nil
}

// DTR returns the last state the DTR line was commanded to; Windows offers
// no way to read the line back.
func (p *Port) DTR() (bool, error) {
	return p.dtr, nil
}

// RTS returns the last state the RTS line was commanded to; Windows offers
// no way to read the line back.
func (p *Port) RTS() (bool, error) {
	return p.rts, nil
}

// DCD returns the status of the Data Carrier Detect line of the port.
func (p *Port) DCD() (bool, error) {
	var status uint32
	r, _, err := syscall.Syscall(nGetCommModemStatus, 2, uintptr(p.fd), uintptr(unsafe.Pointer(&status)), 0)
	if r == 0 {
		return false, errtrace.Wrap(err)
	}
	return status&msRLSDOn > 0, nil
}

func purgeComm(h syscall.Handle, flags uint32) error {
	r, _, err := syscall.Syscall(nPurgeComm, 2, uintptr(h), uintptr(flags), 0)
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

func escapeCommFunction(h syscall.Handle, code int) error {
	r, _, err := syscall.Syscall(nEscapeCommFunction, 2, uintptr(h), uintptr(code), 0)
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

// SetDeadline sets the absolute time after which Read and Write fail with
// os.ErrDeadlineExceeded, matching the os.File deadline behaviour the POSIX
// ports get for free. A zero value means no deadline. Unlike os.File,
// changing the deadline does not interrupt an operation that is already
// blocked in Read or Write; the new deadline applies from the next call.
func (p *Port) SetDeadline(t time.Time) error {
	if p == nil || p.f == nil {
		return errtrace.Wrap(fmt.Errorf("Invalid port on set deadline %v %v", p, p.f))
	}
	p.dlMu.Lock()
	p.deadline = t
	p.dlMu.Unlock()
	return nil
}

// waitTimeout converts the port deadline into a WaitForSingleObject timeout
// in milliseconds: INFINITE when no deadline is set, os.ErrDeadlineExceeded
// (unwrapped; callers wrap) when it has already passed.
func (p *Port) waitTimeout() (uint32, error) {
	p.dlMu.Lock()
	deadline := p.deadline
	p.dlMu.Unlock()

	if deadline.IsZero() {
		return syscall.INFINITE, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, os.ErrDeadlineExceeded
	}
	// Round up so a deadline a few hundred microseconds out still waits,
	// and cap below INFINITE (0xFFFFFFFF), which would mean "no timeout".
	ms := (remaining + time.Millisecond - 1) / time.Millisecond
	if ms >= syscall.INFINITE {
		ms = syscall.INFINITE - 1
	}
	return uint32(ms), nil
}

// waitOverlapped waits for an in-flight overlapped operation to complete,
// enforcing the deadline timeout computed by waitTimeout. On timeout the
// operation is cancelled and os.ErrDeadlineExceeded returned; bytes that were
// transferred before the cancel landed are returned as a successful short
// operation instead, so no data is dropped.
func (p *Port) waitOverlapped(o *syscall.Overlapped, timeout uint32) (int, error) {
	// Close may have set closing and issued its CancelIoEx before this
	// operation started; cancel it ourselves so Close isn't left waiting
	// on the I/O lock we hold.
	if p.closing.Load() {
		syscall.CancelIoEx(p.fd, o)
	}

	s, err := syscall.WaitForSingleObject(o.HEvent, timeout)
	switch s {
	case syscall.WAIT_OBJECT_0:
		// Completed; reap the result below.
	case syscall.WAIT_TIMEOUT:
		// Deadline hit. Cancel and still reap: the operation can complete
		// successfully in the window before the cancel lands.
		syscall.CancelIoEx(p.fd, o)
	default:
		// The wait itself failed, but the operation may still be in
		// flight; cancel and reap it so the kernel isn't left writing
		// into the caller's buffer after we return.
		syscall.CancelIoEx(p.fd, o)
		getOverlappedResult(p.fd, o)
		if err == nil {
			err = syscall.EINVAL
		}
		return 0, errtrace.Wrap(err)
	}

	n, err := getOverlappedResult(p.fd, o)
	if err == errOperationAborted {
		if n > 0 {
			return n, nil
		}
		if p.closing.Load() {
			return 0, errtrace.Wrap(os.ErrClosed)
		}
		return 0, errtrace.Wrap(os.ErrDeadlineExceeded)
	}
	return n, errtrace.Wrap(err)
}

func resetEvent(h syscall.Handle) error {
	r, _, err := syscall.Syscall(nResetEvent, 1, uintptr(h), 0, 0)
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

// getOverlappedResult reaps a completed (or cancelled) overlapped operation,
// waiting for it to finish if it hasn't yet (bWait=TRUE). The error is
// returned unwrapped so callers can compare it against syscall.Errno values.
func getOverlappedResult(h syscall.Handle, overlapped *syscall.Overlapped) (int, error) {
	var n uint32
	r, _, err := syscall.Syscall6(nGetOverlappedResult, 4,
		uintptr(h),
		uintptr(unsafe.Pointer(overlapped)),
		uintptr(unsafe.Pointer(&n)), 1, 0, 0)
	if r == 0 {
		return int(n), err
	}

	return int(n), nil
}
