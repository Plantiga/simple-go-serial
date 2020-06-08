package serial

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// NCCS is the number of control character sequences used for c_cc
const (
	IOSSIOSPEED = 0x80045402
)

// makeTermios returns a pointer to an instantiates termios2 struct, based on the given
// OpenOptions. Termios is a Linux extension which allows arbitrary baud rates
// to be specified.
func makeTermios(fd uintptr, options OpenOptions) (*unix.Termios, error) {

	t := &unix.Termios{}

	err := unix.IoctlSetTermios(int(fd), unix.TIOCGETA, t)
	if err != nil {
		fmt.Println("TCGETS openInternal err")
		return nil, err
	}

	t.Cflag |= (syscall.CLOCAL | syscall.CREAD)
	t.Lflag &= ^uint64(
		syscall.ICANON | syscall.ECHO | syscall.ECHOE |
			syscall.ECHOK | syscall.ECHONL |
			syscall.ISIG | syscall.IEXTEN)
	t.Lflag &= ^uint64(syscall.ECHOCTL)
	t.Lflag &= ^uint64(syscall.ECHOKE)

	t.Oflag &= ^uint64(syscall.OPOST | syscall.ONLCR | syscall.OCRNL)
	t.Iflag &= ^uint64(syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IGNBRK)
	t.Iflag &= ^uint64(syscall.PARMRK)

	// character size
	t.Cflag &= ^uint64(syscall.CSIZE)
	t.Cflag |= uint64(syscall.CS8)

	// setup stop bits
	t.Cflag &= ^uint64(syscall.CSTOPB)

	// setup parity
	t.Iflag &= ^uint64(syscall.INPCK | syscall.ISTRIP)
	t.Cflag &= ^uint64(syscall.PARENB | syscall.PARODD)

	t.Iflag &= ^uint64(syscall.IXON | syscall.IXOFF | syscall.IXANY)

	// // Sanity check inter-character timeout and minimum read size options.
	// // See serial.go for more information on vtime/vmin -- these only work in non-canonical mode
	vtime := uint(round(float64(options.InterCharacterTimeout)/100.0) * 100)
	vmin := options.MinimumReadSize

	t.Cc[syscall.VTIME] = uint8(vtime / 100)
	t.Cc[unix.VMIN] = uint8(vmin)

	return t, nil
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

	t, optErr := makeTermios(fd, options)
	if optErr != nil {
		return nil, optErr
	}

	// Set our termios struct as the file descriptor's settings
	err := unix.IoctlSetTermios(int(fd), unix.TIOCSETA, t)
	if err != nil {
		return nil, err
	}
	b := uint(options.BaudRate)
	errcode := ioctl(IOSSIOSPEED, fd, uintptr(unsafe.Pointer(&b)))
	if errcode != nil {
		return nil, errcode
	}

	return NewPort(file, fd, options), nil
}
