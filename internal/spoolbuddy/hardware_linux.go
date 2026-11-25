//go:build linux

package spoolbuddy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func envInt(k string, defaultValue int) int {
	n, e := strconv.Atoi(os.Getenv(k))
	if e == nil && n >= 0 {
		return n
	}
	return defaultValue
}
func PiHardware() Hardware { return Hardware{OpenScale: openScale, OpenNFC: openNFC} }

type nau7802 struct{ f *os.File }

func (p *nau7802) Close() error { return p.f.Close() }
func (p *nau7802) read(addr byte, buf []byte) error {
	if _, e := p.f.Write([]byte{addr}); e != nil {
		return e
	}
	_, e := io.ReadFull(p.f, buf)
	return e
}
func (p *nau7802) bits(addr, mask, val byte) error {
	var b [1]byte
	if e := p.read(addr, b[:]); e != nil {
		return e
	}
	_, e := p.f.Write([]byte{addr, b[0]&^mask | val&mask})
	return e
}
func (p *nau7802) wait(addr, mask, val byte, timeout time.Duration) error {
	until := time.Now().Add(timeout)
	for time.Now().Before(until) {
		var b [1]byte
		if e := p.read(addr, b[:]); e != nil {
			return e
		}
		if b[0]&mask == val {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("NAU7802 response timeout")
}
func (p *nau7802) Raw() (int, bool, error) {
	var status [1]byte
	if e := p.read(0, status[:]); e != nil {
		return 0, false, e
	}
	if status[0]&0x20 == 0 {
		return 0, false, nil
	}
	var v [3]byte
	e := p.read(0x12, v[:])
	return signedADC(v), e == nil, e
}
func (p *nau7802) flush(n int) error {
	for range n {
		if e := p.wait(0, 0x20, 0x20, time.Second); e != nil {
			return e
		}
		if _, _, e := p.Raw(); e != nil {
			return e
		}
	}
	return nil
}
func openScale() (Scale, error) {
	path := fmt.Sprintf("/dev/i2c-%d", envInt("SPOOLBUDDY_I2C_BUS", 1))
	f, e := os.OpenFile(path, os.O_RDWR, 0)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	if e = unix.IoctlSetInt(int(f.Fd()), 0x0703, 0x2a); e != nil {
		return nil, e
	}
	p := &nau7802{f}
	if e = p.bits(0, 1, 1); e != nil {
		return nil, e
	}
	time.Sleep(10 * time.Millisecond)
	if e = p.bits(0, 1, 0); e != nil {
		return nil, e
	}
	time.Sleep(time.Millisecond)
	if e = p.bits(0, 6, 6); e != nil {
		return nil, e
	}
	if e = p.wait(0, 8, 8, 100*time.Millisecond); e != nil {
		return nil, e
	}
	if e = p.wait(0x1f, 0xf, 0xf, 5*time.Millisecond); e != nil {
		return nil, errors.New("unexpected NAU7802 revision")
	}
	for _, r := range [][3]byte{{0, 0x80, 0x80}, {1, 0x3f, 0x2f}, {2, 0x70, 0}, {0x15, 0x30, 0x30}, {0x1b, 0x40, 0}, {0, 0x10, 0x10}} {
		if e = p.bits(r[0], r[1], r[2]); e != nil {
			return nil, e
		}
	}
	if e = p.flush(4); e != nil {
		return nil, e
	}
	if e = p.bits(2, 7, 4); e != nil {
		return nil, e
	}
	if e = p.wait(2, 0x0c, 0, time.Second); e != nil {
		return nil, errors.New("NAU7802 calibration failed")
	}
	if e = p.flush(2); e != nil {
		return nil, e
	}
	ok = true
	return p, nil
}

// Linux GPIO character-device v1 and spidev layouts are fixed-width UAPI structures on the Pi.
type lineRequest struct {
	Offsets  [64]uint32
	Flags    uint32
	Default  [64]byte
	Consumer [32]byte
	Lines    uint32
	FD       int32
}

func ioctl(fd uintptr, op uintptr, ptr unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, fd, op, uintptr(ptr))
	if e != 0 {
		return e
	}
	return nil
}

type gpioLine struct{ f *os.File }

func line(chip *os.File, pin int, output bool) (*gpioLine, error) {
	q := lineRequest{Lines: 1, Flags: 1}
	q.Offsets[0] = uint32(pin)
	copy(q.Consumer[:], "spoolbuddy-pn5180")
	if output {
		q.Flags = 2
		q.Default[0] = 1
	}
	if e := ioctl(chip.Fd(), 0xc16cb403, unsafe.Pointer(&q)); e != nil {
		return nil, e
	}
	return &gpioLine{os.NewFile(uintptr(q.FD), "gpio-line")}, nil
}
func (l *gpioLine) set(on bool) error {
	var data [64]byte
	if on {
		data[0] = 1
	}
	return ioctl(l.f.Fd(), 0xc040b409, unsafe.Pointer(&data))
}
func (l *gpioLine) value() (bool, error) {
	var data [64]byte
	e := ioctl(l.f.Fd(), 0xc040b408, unsafe.Pointer(&data))
	return data[0] != 0, e
}

type spiTransfer struct {
	TX, RX                                   uint64
	Length, Speed                            uint32
	Delay                                    uint16
	Bits, CS, TxBits, RxBits, WordDelay, Pad uint8
}
type spi struct {
	file                    *os.File
	busy, reset, selectLine *gpioLine
	speed                   uint32
}

func (b *spi) Close() error {
	for _, l := range []*gpioLine{b.busy, b.reset, b.selectLine} {
		if l != nil {
			l.f.Close()
		}
	}
	if b.file != nil {
		return b.file.Close()
	}
	return nil
}
func (b *spi) busyWait() error {
	start := time.Now()
	for time.Since(start) < 10*time.Millisecond {
		v, e := b.busy.value()
		if e != nil {
			return e
		}
		if v {
			break
		}
		time.Sleep(10 * time.Microsecond)
	}
	for time.Since(start) < time.Second {
		v, e := b.busy.value()
		if e != nil {
			return e
		}
		if !v {
			return nil
		}
		time.Sleep(100 * time.Microsecond)
	}
	return errors.New("PN5180 busy timeout")
}
func (b *spi) transfer(tx []byte) ([]byte, error) {
	rx := make([]byte, len(tx))
	r := spiTransfer{TX: uint64(uintptr(unsafe.Pointer(&tx[0]))), RX: uint64(uintptr(unsafe.Pointer(&rx[0]))), Length: uint32(len(tx)), Speed: b.speed, Bits: 8}
	if e := b.selectLine.set(false); e != nil {
		return nil, e
	}
	time.Sleep(5 * time.Microsecond)
	e := ioctl(b.file.Fd(), 0x40206b00, unsafe.Pointer(&r))
	runtime.KeepAlive(tx)
	runtime.KeepAlive(rx)
	csErr := b.selectLine.set(true)
	if e != nil {
		return nil, e
	}
	time.Sleep(100 * time.Microsecond)
	return rx, csErr
}
func (b *spi) Command(v []byte, n int) ([]byte, error) {
	if _, e := b.transfer(v); e != nil {
		return nil, e
	}
	if e := b.busyWait(); e != nil {
		return nil, e
	}
	if n == 0 {
		return nil, nil
	}
	return b.transfer(bytes.Repeat([]byte{0xff}, n))
}
func gpioDevice() string {
	if v := os.Getenv("SPOOLBUDDY_GPIO_CHIP"); v != "" {
		return v
	}
	files, _ := filepath.Glob("/sys/bus/gpio/devices/gpiochip*/label")
	for _, f := range files {
		label, e := os.ReadFile(f)
		if e == nil && (strings.Contains(string(label), "pinctrl-rp1") || strings.Contains(string(label), "pinctrl-bcm")) {
			return filepath.Join("/dev", filepath.Base(filepath.Dir(f)))
		}
	}
	return "/dev/gpiochip0"
}
func openNFC() (NFC, error) {
	chip, e := os.OpenFile(gpioDevice(), os.O_RDWR, 0)
	if e != nil {
		return nil, e
	}
	defer chip.Close()
	b := &spi{speed: uint32(envInt("SPOOLBUDDY_NFC_SPI_SPEED_HZ", 500000))}
	ok := false
	defer func() {
		if !ok {
			b.Close()
		}
	}()
	b.busy, e = line(chip, envInt("SPOOLBUDDY_NFC_BUSY_PIN", 25), false)
	if e != nil {
		return nil, e
	}
	b.reset, e = line(chip, envInt("SPOOLBUDDY_NFC_RST_PIN", 24), true)
	if e != nil {
		return nil, e
	}
	b.selectLine, e = line(chip, envInt("SPOOLBUDDY_NFC_NSS_PIN", 23), true)
	if e != nil {
		return nil, e
	}
	path := fmt.Sprintf("/dev/spidev%d.%d", envInt("SPOOLBUDDY_NFC_SPI_BUS", 0), envInt("SPOOLBUDDY_NFC_SPI_DEVICE", 0))
	b.file, e = os.OpenFile(path, os.O_RDWR, 0)
	if e != nil {
		return nil, e
	}
	mode := byte(0)
	if e = ioctl(b.file.Fd(), 0x40016b01, unsafe.Pointer(&mode)); e != nil {
		return nil, e
	}
	mode = 0x40
	_ = ioctl(b.file.Fd(), 0x40016b01, unsafe.Pointer(&mode))
	if e = b.reset.set(false); e != nil {
		return nil, e
	}
	time.Sleep(50 * time.Millisecond)
	if e = b.reset.set(true); e != nil {
		return nil, e
	}
	time.Sleep(100 * time.Millisecond)
	if e = b.busyWait(); e != nil {
		return nil, e
	}
	time.Sleep(50 * time.Millisecond)
	p := &pn5180{b}
	if e = p.reg(0, 3, ^uint32(0)); e != nil {
		return nil, e
	}
	if _, e = b.Command([]byte{0x11, 0, 0x80}, 0); e != nil {
		return nil, e
	}
	time.Sleep(10 * time.Millisecond)
	if _, e = b.Command([]byte{0x16, 0}, 0); e != nil {
		return nil, e
	}
	time.Sleep(30 * time.Millisecond)
	if e = p.mode(); e != nil {
		return nil, e
	}
	ok = true
	return p, nil
}
