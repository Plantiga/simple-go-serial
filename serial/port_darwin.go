package serial

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Port represents a File opened with serial port options
type Port struct {
	f          *os.File
	fd         uintptr
	DeviceName string
}

// Read reads up to len(b) bytes from the Port's file.
// It will return the number of bytes read and an error, if any
func (p *Port) Read(b []byte) (int, error) {
	return p.f.Read(b)
}

// Write writes len(b) number of bytes to the Port's file.
// It will return the number of bytes written and an error, if any
func (p *Port) Write(b []byte) (int, error) {
	return p.f.Write(b)
}

// Close closes the Port's file, making it unusable for I/O
func (p *Port) Close() error {
	return p.f.Close()
}

// var FIONREAD = 0x541B
var TIOCINQ = 0x4004667f

// InWaiting returns the number of waiting bytes in the Port's internal buffer.
func (p *Port) InWaiting() (int, error) {
	// Funky time
	var waiting int
	err := ioctl(TIOCINQ, p.fd, uintptr(unsafe.Pointer(&waiting)))
	if err != nil {
		return 0, err
	}
	return waiting, nil
}

var TCFLSH = 0x540b

func (p *Port) ResetInputBuffer() error {
	return ioctl(TCFLSH, p.fd, unix.TCIFLUSH)
}

func (p *Port) ResetOutputBuffer() error {
	return ioctl(TCFLSH, p.fd, unix.TCOFLUSH)
}

// SetDeadline sets the read and write deadlines for the Port's file.
// Deadlines are absolute timeouts after which any read or write calls will fail with a timeout error.
func (p *Port) SetDeadline(t time.Time) error {
	// Funky Town
	err := p.f.SetDeadline(t)
	if err != nil {
		return err
	}
	return nil
}

// DTR returns the status of the Data Terminal Ready (DTR) line of the port.
// See: https://en.wikipedia.org/wiki/Data_Terminal_Ready
func (p *Port) DTR() (bool, error) {
	var status int
	err := ioctl(unix.TIOCMGET, p.fd, uintptr(unsafe.Pointer(&status)))
	if err != nil {
		return false, err
	}
	if status&unix.TIOCM_DTR > 0 {
		return true, nil
	}
	return false, nil
}

// RTS
// See: https://en.wikipedia.org/wiki/Data_Terminal_Ready
func (p *Port) RTS() (bool, error) {
	var status int
	err := ioctl(unix.TIOCMGET, p.fd, uintptr(unsafe.Pointer(&status)))
	if err != nil {
		return false, err
	}
	if status&unix.TIOCM_RTS > 0 {
		return true, nil
	}
	return false, nil
}

// SetDTR sets the status of the DTR line of a port to the given state,
// allowing manual control of the Data Terminal Ready modem line.
func (p *Port) SetDTR(state bool) error {
	var command int
	dtrFlag := unix.TIOCM_DTR
	if state {
		command = unix.TIOCMBIS
	} else {
		command = unix.TIOCMBIC
	}
	err := ioctl(command, p.fd, uintptr(unsafe.Pointer(&dtrFlag)))
	if err != nil {
		return err
	}
	return nil
}

// SetRTS sets the status of the RTS line of a port to the given state,
func (p *Port) SetRTS(state bool) error {
	var command int
	flag := unix.TIOCM_RTS
	if state {
		command = unix.TIOCMBIS
	} else {
		command = unix.TIOCMBIC
	}
	err := ioctl(command, p.fd, uintptr(unsafe.Pointer(&flag)))
	if err != nil {
		return err
	}
	return nil
}

// NewPort creates and returns a new Port struct using the given os.File pointer
func NewPort(f *os.File, fd uintptr, options OpenOptions) *Port {
	return &Port{f, fd, options.PortName}
}
