package serial

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// NCCS is the number of control character sequences used for c_cc
const (
	nccs        = 20
	iossiospeed = 0x80045402
)

var (
	standardBaudRates = map[uint]bool{
		50:     true,
		75:     true,
		110:    true,
		134:    true,
		150:    true,
		200:    true,
		300:    true,
		600:    true,
		1200:   true,
		1800:   true,
		2400:   true,
		4800:   true,
		7200:   true,
		9600:   true,
		14400:  true,
		19200:  true,
		28800:  true,
		38400:  true,
		57600:  true,
		76800:  true,
		115200: true,
		230400: true,
	}
)

// Types from asm-generic/termbits.h
type cc_t byte
type speed_t uint32
type tcflag_t uint32
type termios struct {
	c_iflag  tcflag_t   // input mode flags
	c_oflag  tcflag_t   // output mode flags
	c_cflag  tcflag_t   // control mode flags
	c_lflag  tcflag_t   // local mode flags
	c_cc     [nccs]cc_t // control characters
	c_ispeed speed_t    // input speed
	c_ospeed speed_t    // output speed
}

func isStandardBaudrate(baudrate uint) bool {
	return standardBaudRates[baudrate]
}

// makeTermios returns a pointer to an instantiates termios struct, based on the given
// OpenOptions.
func makeTermios(options OpenOptions) (*termios, error) {

	// Sanity check inter-character timeout and minimum read size options.
	// See serial.go for more information on vtime/vmin -- these only work in non-canonical mode
	vtime := uint(round(float64(options.InterCharacterTimeout)/100.0) * 100)
	vmin := options.MinimumReadSize

	if vmin == 0 && vtime < 100 {
		return nil, errors.New("invalid values for InterCharacterTimeout and MinimumReadSize")
	}

	if vtime > 25500 {
		return nil, errors.New("invalid value for InterCharacterTimeout")
	}

	ccOpts := [nccs]cc_t{}
	ccOpts[unix.VTIME] = cc_t(vtime / 100)
	ccOpts[unix.VMIN] = cc_t(vmin)

	baudrate := options.BaudRate
	if !isStandardBaudrate(baudrate) {
		// Set an arbitrary baudrate in the termios struct, we will set this later with a, iossiospeed system call
		baudrate = 115200
	}
	// We set the flags for CLOCAL, CREAD and BOTHER
	// CLOCAL : ignore modem control lines
	// CREAD  : enable receiver
	term := &termios{
		c_cflag:  unix.CLOCAL | unix.CREAD,
		c_ispeed: speed_t(baudrate),
		c_ospeed: speed_t(baudrate),
		c_cc:     ccOpts,
	}

	// Un-set the ICANON mode to allow non-canonical mode
	// See: https://www.gnu.org/software/libc/manual/html_node/Canonical-or-Not.html
	if !options.CanonicalMode {
		term.c_lflag &= ^tcflag_t(unix.ICANON)
	}

	// Allow for setting 1 or 2 stop bits
	switch options.StopBits {
	case 1:
	case 2:
		term.c_cflag |= unix.CSTOPB

	default:
		return nil, errors.New("invalid setting for StopBits")
	}

	// If odd or even, enable parity generation (PARENB) and determine the type
	switch options.ParityMode {
	case Parity_None:
	case Parity_Odd:
		term.c_cflag |= unix.PARENB
		term.c_cflag |= unix.PARODD

	case Parity_Even:
		term.c_cflag |= unix.PARENB

	default:
		return nil, errors.New("invalid setting for ParityMode")
	}

	// Choose the databits per frame
	switch options.DataBits {
	case 5:
		term.c_cflag |= unix.CS5
	case 6:
		term.c_cflag |= unix.CS6
	case 7:
		term.c_cflag |= unix.CS7
	case 8:
		term.c_cflag |= unix.CS8
	default:
		return nil, errors.New("invalid setting for DataBits")
	}

	return term, nil
}

// openInternal is the operating system specific port opening, given the OpenOptions
func openInternal(options OpenOptions) (*Port, error) {
	// Open the file with RDWR, NOCTTY, NONBLOCK flags
	// RDWR     : read/write
	// NOCTTY   : don't allow the port to become the controlling terminal
	// NONBLOCK : open with nonblocking so we don't stall
	file, openErr :=
		os.OpenFile(
			options.PortName,
			unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK,
			0777)
	if openErr != nil {
		return nil, openErr
	}

	fd := file.Fd()

	// When we call Fd(), we make the file descriptor blocking, which we don't want
	// Let's unset the blocking flag and save the pointer for later.
	nonblockErr := unix.SetNonblock(int(fd), true)
	if nonblockErr != nil {
		return nil, nonblockErr
	}

	term, optErr := makeTermios(options)
	if optErr != nil {
		return nil, optErr
	}

	// Set our termios struct as the file descriptor's settings
	errno := ioctl(unix.TIOCSETA, fd, uintptr(unsafe.Pointer(term)))
	if errno != nil {
		return nil, errno
	}

	if !isStandardBaudrate(options.BaudRate) {
		speederr := ioctl(iossiospeed, fd, uintptr(unsafe.Pointer(&options.BaudRate)))
		if speederr != nil {
			return nil, speederr
		}
	}

	return NewPort(file, fd, options), nil
}
