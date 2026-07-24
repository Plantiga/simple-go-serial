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
	return errtrace.Wrap(p.f.Close())
}

func (p *Port) InWaiting() (int, error) {
	return 0, nil
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
	return errtrace.Wrap(escapeCommFunction(p.fd, code))
}

// SetRTS sets the status of the RTS line of a port to the given state.
func (p *Port) SetRTS(state bool) error {
	code := escapeClrRTS
	if state {
		code = escapeSetRTS
	}
	return errtrace.Wrap(escapeCommFunction(p.fd, code))
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
