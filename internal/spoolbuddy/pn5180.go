package spoolbuddy

import (
	"bambastic/internal/opentag"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// The PN5180 host protocol sends commands and clocks out replies in separate chip-select windows.
type pnBus interface {
	Command([]byte, int) ([]byte, error)
	Close() error
}
type pn5180 struct{ bus pnBus }

func (p *pn5180) Close() error { _, _ = p.bus.Command([]byte{0x17, 0}, 0); return p.bus.Close() }
func (p *pn5180) reg(op, addr byte, value uint32) error {
	q := []byte{op, addr, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(q[2:], value)
	_, e := p.bus.Command(q, 0)
	return e
}
func (p *pn5180) read(addr byte) (uint32, error) {
	v, e := p.bus.Command([]byte{4, addr}, 4)
	if e != nil {
		return 0, e
	}
	if len(v) != 4 {
		return 0, errors.New("short PN5180 register read")
	}
	return binary.LittleEndian.Uint32(v), nil
}
func (p *pn5180) mode() error {
	v, e := p.read(0)
	if e != nil {
		return e
	}
	return p.reg(0, 0, v&^7|3)
}
func (p *pn5180) frame(frame []byte, bits byte, wait time.Duration, maxBytes int) ([]byte, error) {
	if e := p.reg(0, 3, ^uint32(0)); e != nil {
		return nil, e
	}
	if e := p.mode(); e != nil {
		return nil, e
	}
	time.Sleep(2 * time.Millisecond)
	if _, e := p.bus.Command(append([]byte{9, bits}, frame...), 0); e != nil {
		return nil, e
	}
	time.Sleep(wait)
	status, e := p.read(0x13)
	if e != nil {
		return nil, e
	}
	n := int(status & 511)
	if n == 0 || n == 511 || n > 64 {
		return nil, nil
	}
	return p.bus.Command([]byte{0x0a, 0}, min(n, maxBytes))
}
func (p *pn5180) crc(enable bool) error {
	op := byte(2)
	mask := ^uint32(1)
	if enable {
		op = 1
		mask = 1
	}
	if e := p.reg(op, 0x12, mask); e != nil {
		return e
	}
	return p.reg(op, 0x19, mask)
}
func (p *pn5180) ReadTag() (*Tag, error) {
	if e := p.reg(2, 0, ^uint32(0x40)); e != nil {
		return nil, e
	}
	if e := p.crc(false); e != nil {
		return nil, e
	}
	if e := p.reg(2, 0, ^uint32(7)); e != nil {
		return nil, e
	}
	for i, cmd := range []byte{0x52, 0x26} {
		resp, e := p.frame([]byte{cmd}, 7, 5*time.Millisecond, 2)
		if e != nil {
			return nil, e
		}
		if len(resp) == 2 && resp[0] != 0xff && resp[0] != 0 {
			break
		}
		if i == 1 {
			return nil, nil
		}
	}
	uid := []byte{}
	for _, sel := range []byte{0x93, 0x95, 0x97} {
		if e := p.crc(false); e != nil {
			return nil, e
		}
		part, e := p.frame([]byte{sel, 0x20}, 0, 10*time.Millisecond, 5)
		if e != nil {
			return nil, e
		}
		if len(part) != 5 || part[0]^part[1]^part[2]^part[3] != part[4] {
			return nil, nil
		}
		if e = p.crc(true); e != nil {
			return nil, e
		}
		sak, e := p.frame(append([]byte{sel, 0x70}, part...), 0, 10*time.Millisecond, 3)
		if e != nil {
			return nil, e
		}
		if len(sak) == 0 {
			return nil, nil
		}
		if sak[0]&4 == 0 {
			uid = append(uid, part[:4]...)
			return &Tag{strings.ToUpper(hex.EncodeToString(uid)), sak[0]}, nil
		}
		if part[0] != 0x88 {
			return nil, nil
		}
		uid = append(uid, part[1:4]...)
	}
	return nil, nil
}
func signedADC(b [3]byte) int {
	v := int32(b[0])<<16 | int32(b[1])<<8 | int32(b[2])
	return int(v << 8 >> 8)
}

func validRecord(d []byte) bool { return opentag.Payload(d) != nil }

// WriteTag only touches user data pages on a formatted, writable Type 2 tag. Compare its UID
// before writing and verify every byte after re-selection before linking the tag to a spool.
func (p *pn5180) WriteTag(uid string, short, large []byte) (string, error) {
	if !validRecord(short) || len(large) > 0 && !validRecord(large) {
		return "", errors.New("invalid OpenTag3D NDEF payload")
	}
	prepare := func() error {
		if e := p.reg(2, 0, ^uint32(0x40)); e != nil {
			return e
		}
		if e := p.reg(1, 0x19, 1); e != nil {
			return e
		}
		return p.reg(2, 0x12, ^uint32(1))
	}
	reselect := func() error {
		if _, e := p.bus.Command([]byte{0x17, 0}, 0); e != nil {
			return e
		}
		time.Sleep(7 * time.Millisecond)
		if _, e := p.bus.Command([]byte{0x16, 0}, 0); e != nil {
			return e
		}
		time.Sleep(15 * time.Millisecond)
		tag, e := p.ReadTag()
		if e != nil {
			return e
		}
		if tag == nil || tag.UID != uid || (tag.SAK != 0 && tag.SAK != 4) {
			return errors.New("presented tag changed")
		}
		return prepare()
	}
	readPage := func(page byte) ([]byte, error) {
		b, e := p.frame([]byte{0x30, page}, 0, 10*time.Millisecond, 16)
		if e != nil {
			return nil, e
		}
		if len(b) != 16 {
			return nil, errors.New("incomplete NTAG read")
		}
		return b, nil
	}
	if e := reselect(); e != nil {
		return "", e
	}
	cc, e := readPage(3)
	if e != nil {
		return "", e
	}
	if cc[0] != 0xe1 || cc[1]>>4 != 1 || cc[3]&15 != 0 {
		return "", errors.New("tag is unformatted or read-only")
	}
	capacity := int(cc[2]) * 8
	data := short
	version := "1.000"
	if len(large) > 0 && len(large) <= capacity {
		data = large
		version = "2.004"
	}
	if len(data) > capacity {
		return "", errors.New("tag is too small for the spool record")
	}
	padded := make([]byte, (len(data)+3)/4*4)
	copy(padded, data)
	for off := 0; off < len(padded); off += 4 {
		page := byte(4 + off/4)
		if _, e = p.frame(append([]byte{0xa2, page}, padded[off:off+4]...), 0, 6*time.Millisecond, 1); e != nil {
			return "", e
		}
	}
	if e = reselect(); e != nil {
		return "", errors.New("written tag could not be reselected for verification")
	}
	for off := 0; off < len(padded); off += 16 {
		b, e := readPage(byte(4 + off/4))
		if e != nil {
			return "", e
		}
		n := min(16, len(padded)-off)
		if !bytes.Equal(b[:n], padded[off:off+n]) {
			return "", errors.New("NTAG read-back does not match the requested record")
		}
	}
	return "Verified OpenTag3D " + version, nil
}
