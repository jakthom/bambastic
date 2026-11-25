// Optional metadata reads run once when a tag enters the field. A failed
// read still leaves the original UID available for inventory matching.
package spoolbuddy

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type tagMetadataReader interface{ TagMetadata(*Tag) map[string]any }

type sectorCredential struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

func configuredSectors(uid string) map[string]sectorCredential {
	file := os.Getenv("SPOOLBUDDY_TAG_KEYS_FILE")
	if file == "" {
		return nil
	}
	f, e := os.Open(file)
	if e != nil {
		return nil
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil
	}
	var tags map[string]map[string]sectorCredential
	if e = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&tags); e != nil {
		return nil
	}
	return tags[strings.ToUpper(uid)]
}

func (p *pn5180) readDataBlock(n byte) []byte {
	v, e := p.frame([]byte{0x30, n}, 0, 10*time.Millisecond, 16)
	if e != nil || len(v) != 16 {
		return nil
	}
	return v
}

func (p *pn5180) TagMetadata(t *Tag) map[string]any {
	if t.SAK == 0 || t.SAK == 4 {
		out := map[string]any{"tag_type": "ntag"}
		if e := p.crc(true); e != nil {
			return out
		}
		cc := p.readDataBlock(3)
		if len(cc) != 16 || cc[0] != 0xe1 || cc[1]>>4 != 1 || cc[3]>>4 != 0 {
			return out
		}
		first := p.readDataBlock(4)
		if len(first) != 16 || first[0] != 3 || first[1] == 0xff {
			return out
		}
		size := int(first[1]) + 3
		if size < 16 || size > int(cc[2])*8 {
			return out
		}
		data := append([]byte(nil), first...)
		for len(data) < size {
			block := p.readDataBlock(byte(4 + len(data)/4))
			if block == nil {
				return out
			}
			data = append(data, block...)
		}
		if validRecord(data[:size]) {
			out["tag_type"] = "OpenTag3D"
			out["opentag3d_ndef"] = hex.EncodeToString(data[:size])
		}
		return out
	}
	if t.SAK != 8 && t.SAK != 0x18 {
		return map[string]any{"tag_type": "unknown"}
	}
	out := map[string]any{"tag_type": "mifare_classic"}
	sectors := configuredSectors(t.UID)
	uid, e := hex.DecodeString(t.UID)
	if len(sectors) == 0 || e != nil || len(uid) != 4 {
		return out
	}
	if e = p.crc(true); e != nil {
		return out
	}
	blocks := map[string]string{}
	// These are spool data blocks. Never read or transmit sector trailers.
	for sector, addresses := range [][]byte{{1, 2}, {4, 5, 6}, {9}} {
		cred, ok := sectors[strconv.Itoa(sector)]
		if !ok {
			continue
		}
		key, e := hex.DecodeString(cred.Key)
		if e != nil || len(key) != 6 {
			continue
		}
		var kind byte
		switch strings.ToUpper(cred.Type) {
		case "A":
			kind = 0x60
		case "B":
			kind = 0x61
		default:
			continue
		}
		cmd := append([]byte{0x0c}, key...)
		cmd = append(cmd, kind, addresses[0])
		cmd = append(cmd, uid...)
		status, e := p.bus.Command(cmd, 1)
		if e != nil || len(status) != 1 || status[0] != 0 {
			break
		}
		for _, n := range addresses {
			data := p.readDataBlock(n)
			if data == nil {
				continue
			}
			value := strings.ToUpper(hex.EncodeToString(data))
			blocks[strconv.Itoa(int(n))] = value
			if n == 9 && !bytes.Equal(data, make([]byte, 16)) {
				out["tray_uuid"] = value
			}
		}
	}
	if len(blocks) > 0 {
		out["raw_blocks"] = blocks
	}
	return out
}
