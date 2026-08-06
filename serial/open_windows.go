// Copyright 2011 Aaron Jacobs. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This portion of the package has no automated tests; it is exercised
// against real hardware (Plantiga docks) on Windows.

package serial

import (
	"braces.dev/errtrace"
	"os"
	"syscall"
	"unsafe"
)

type structDCB struct {
	DCBlength, BaudRate                            uint32
	flags                                          [4]byte
	wReserved, XonLim, XoffLim                     uint16
	ByteSize, Parity, StopBits                     byte
	XonChar, XoffChar, ErrorChar, EofChar, EvtChar byte
	wReserved1                                     uint16
}

type structTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

func openInternal(options OpenOptions) (*Port, error) {
	if len(options.PortName) > 0 && options.PortName[0] != '\\' {
		options.PortName = "\\\\.\\" + options.PortName
	}

	h, err := syscall.CreateFile(syscall.StringToUTF16Ptr(options.PortName),
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OVERLAPPED,
		0)
	if err != nil {
		return nil, errtrace.Wrap(err)
	}
	f := os.NewFile(uintptr(h), options.PortName)
	defer func() {
		if err != nil {
			f.Close()
		}
	}()

	if err = setCommState(h, options); err != nil {
		return nil, errtrace.Wrap(err)
	}
	// Recommend generous driver-side queues. At high baud rates (the Plantiga
	// dock runs at 3 Mbps) a large burst can arrive faster than user space
	// drains it; a 64-byte RX buffer (the old value) overruns almost instantly
	// and silently drops bytes, corrupting framing downstream.
	if err = setupComm(h, 65536, 65536); err != nil {
		return nil, errtrace.Wrap(err)
	}
	if err = setCommTimeouts(h, options); err != nil {
		return nil, errtrace.Wrap(err)
	}
	if err = setCommMask(h); err != nil {
		return nil, errtrace.Wrap(err)
	}

	ro, err := newOverlapped()
	if err != nil {
		return nil, errtrace.Wrap(err)
	}
	wo, err := newOverlapped()
	if err != nil {
		syscall.CloseHandle(ro.HEvent)
		return nil, errtrace.Wrap(err)
	}
	port := new(Port)
	port.f = f
	port.fd = h
	port.ro = ro
	port.wo = wo
	port.DeviceName = options.PortName
	// Both lines are enabled in the DCB by setCommState.
	port.dtr = true
	port.rts = true

	return port, nil
}

var (
	nSetCommState,
	nSetCommTimeouts,
	nSetCommMask,
	nSetupComm,
	nGetOverlappedResult,
	nCreateEvent,
	nResetEvent,
	nPurgeComm,
	nEscapeCommFunction,
	nGetCommModemStatus,
	nClearCommError uintptr
)

func init() {
	k32, err := syscall.LoadLibrary("kernel32.dll")
	if err != nil {
		panic("LoadLibrary " + err.Error())
	}
	defer syscall.FreeLibrary(k32)

	nSetCommState = getProcAddr(k32, "SetCommState")
	nSetCommTimeouts = getProcAddr(k32, "SetCommTimeouts")
	nSetCommMask = getProcAddr(k32, "SetCommMask")
	nSetupComm = getProcAddr(k32, "SetupComm")
	nGetOverlappedResult = getProcAddr(k32, "GetOverlappedResult")
	nCreateEvent = getProcAddr(k32, "CreateEventW")
	nResetEvent = getProcAddr(k32, "ResetEvent")
	nPurgeComm = getProcAddr(k32, "PurgeComm")
	nEscapeCommFunction = getProcAddr(k32, "EscapeCommFunction")
	nGetCommModemStatus = getProcAddr(k32, "GetCommModemStatus")
	nClearCommError = getProcAddr(k32, "ClearCommError")
}

func getProcAddr(lib syscall.Handle, name string) uintptr {
	addr, err := syscall.GetProcAddress(lib, name)
	if err != nil {
		panic(name + " " + err.Error())
	}
	return addr
}

func setCommState(h syscall.Handle, options OpenOptions) error {
	var params structDCB
	params.DCBlength = uint32(unsafe.Sizeof(params))

	params.flags[0] = 0x01  // fBinary
	params.flags[0] |= 0x10 // fDtrControl = DTR_CONTROL_ENABLE (0x1)
	// POSIX asserts both modem lines when a tty is opened; match that here,
	// devices (e.g. Plantiga docks) key off the host raising them.
	params.flags[1] |= 0x10 // fRtsControl = RTS_CONTROL_ENABLE (0x1)

	if options.ParityMode != Parity_None {
		params.flags[0] |= 0x03 // fParity
		params.Parity = byte(options.ParityMode)
	}

	if options.StopBits == 1 {
		params.StopBits = 0
	} else if options.StopBits == 2 {
		params.StopBits = 2
	}

	params.BaudRate = uint32(options.BaudRate)
	params.ByteSize = byte(options.DataBits)

	r, _, err := syscall.Syscall(nSetCommState, 2, uintptr(h), uintptr(unsafe.Pointer(&params)), 0)
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

// setCommTimeouts maps the POSIX VMIN/VTIME semantics used by the other
// platforms onto Windows COMMTIMEOUTS. Absolute deadlines (SetDeadline) are
// enforced per operation in Read/Write via CancelIoEx, not here.
//
// The magic combination below comes from
// https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-commtimeouts:
// with ReadIntervalTimeout and ReadTotalTimeoutMultiplier both MAXDWORD and
// 0 < ReadTotalTimeoutConstant < MAXDWORD, ReadFile returns immediately if
// bytes are already buffered, otherwise waits up to the constant for the
// first byte to arrive, and times out (0 bytes, no error) if none does.
func setCommTimeouts(h syscall.Handle, options OpenOptions) error {
	var timeouts structTimeouts
	const MAXDWORD = 1<<32 - 1

	switch {
	case options.MinimumReadSize > 0:
		// VMIN > 0: block until data arrives, however long that takes
		// (MAXDWORD-1 ms is ~49 days). A Read under a deadline is cut off
		// by CancelIoEx in waitOverlapped, not by the driver.
		//
		// COMMTIMEOUTS cannot enforce a minimum byte count: ReadFile
		// returns as soon as any byte is buffered, so a MinimumReadSize
		// greater than 1 still yields short reads here.
		timeouts.ReadIntervalTimeout = MAXDWORD
		timeouts.ReadTotalTimeoutMultiplier = MAXDWORD
		timeouts.ReadTotalTimeoutConstant = MAXDWORD - 1
	case options.InterCharacterTimeout > 0:
		// VMIN == 0, VTIME > 0: wait up to the timeout for the first byte,
		// returning whatever is buffered (possibly nothing).
		constant := uint64(options.InterCharacterTimeout)
		if constant >= MAXDWORD {
			constant = MAXDWORD - 1
		}
		timeouts.ReadIntervalTimeout = MAXDWORD
		timeouts.ReadTotalTimeoutMultiplier = MAXDWORD
		timeouts.ReadTotalTimeoutConstant = uint32(constant)
	default:
		// VMIN == 0, VTIME == 0: fully non-blocking, return only what is
		// already buffered. Interval MAXDWORD with both totals zero is the
		// documented combination for that.
		timeouts.ReadIntervalTimeout = MAXDWORD
	}
	// Writes get no driver-side timeout; deadlines cover them too.

	r, _, err := syscall.Syscall(nSetCommTimeouts, 2, uintptr(h), uintptr(unsafe.Pointer(&timeouts)), 0)
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

func setupComm(h syscall.Handle, in, out int) error {
	r, _, err := syscall.Syscall(nSetupComm, 3, uintptr(h), uintptr(in), uintptr(out))
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

func setCommMask(h syscall.Handle) error {
	const EV_RXCHAR = 0x0001
	r, _, err := syscall.Syscall(nSetCommMask, 2, uintptr(h), EV_RXCHAR, 0)
	if r == 0 {
		return errtrace.Wrap(err)
	}
	return nil
}

func newOverlapped() (*syscall.Overlapped, error) {
	var overlapped syscall.Overlapped
	r, _, err := syscall.Syscall6(nCreateEvent, 4, 0, 1, 0, 0, 0, 0)
	if r == 0 {
		return nil, errtrace.Wrap(err)
	}
	overlapped.HEvent = syscall.Handle(r)
	return &overlapped, nil
}
