// This portion of the package is currently untested and not being worked on

package serial

import (
	"braces.dev/errtrace"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

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
}

func (p *Port) Read(buf []byte) (int, error) {
	if p == nil || p.f == nil {
		return 0, errtrace.Wrap(fmt.Errorf("Invalid port on read %v %v", p, p.f))
	}

	p.rl.Lock()
	defer p.rl.Unlock()

	if err := resetEvent(p.ro.HEvent); err != nil {
		return 0, errtrace.Wrap(err)
	}
	var done uint32
	err := syscall.ReadFile(p.fd, buf, &done, p.ro)
	if err != nil && err != syscall.ERROR_IO_PENDING {
		return int(done), errtrace.Wrap(err)
	}
	return errtrace.Wrap2(getOverlappedResult(p.fd, p.ro))
}

func (p *Port) Write(buf []byte) (int, error) {
	p.wl.Lock()
	defer p.wl.Unlock()

	if err := resetEvent(p.wo.HEvent); err != nil {
		return 0, errtrace.Wrap(err)
	}
	var n uint32
	err := syscall.WriteFile(p.fd, buf, &n, p.wo)
	if err != nil && err != syscall.ERROR_IO_PENDING {
		return int(n), errtrace.Wrap(err)
	}
	return errtrace.Wrap2(getOverlappedResult(p.fd, p.wo))
}

func (p *Port) Close() error {
	// Take both IO locks so the event handles aren't closed out from under
	// an in-flight overlapped operation; reads and writes complete within
	// the configured comm timeouts, so the locks settle quickly.
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

func (p *Port) SetDeadline(time.Time) error {
	return nil
}

func resetEvent(h syscall.Handle) error {
	r, _, err := syscall.Syscall(nResetEvent, 1, uintptr(h), 0, 0)
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

func getOverlappedResult(h syscall.Handle, overlapped *syscall.Overlapped) (int, error) {
	var n int
	r, _, err := syscall.Syscall6(nGetOverlappedResult, 4,
		uintptr(h),
		uintptr(unsafe.Pointer(overlapped)),
		uintptr(unsafe.Pointer(&n)), 1, 0, 0)
	if r == 0 {
		return n, errtrace.Wrap(err)
	}

	return n, nil
}
