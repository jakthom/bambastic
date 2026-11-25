// Package opentag recognizes the MIME-encoded OpenTag3D spool record used by the station.
package opentag

func Payload(data []byte) []byte {
	const mime = "application/opentag3d"
	const offset = 5 + len(mime)
	if len(data) < offset+1 || data[0] != 3 || int(data[1]) != len(data)-3 || data[2] != 0xd2 ||
		int(data[3]) != len(mime) || int(data[4]) != len(data)-offset-1 || string(data[5:offset]) != mime ||
		data[len(data)-1] != 0xfe {
		return nil
	}
	return data[offset : len(data)-1]
}
