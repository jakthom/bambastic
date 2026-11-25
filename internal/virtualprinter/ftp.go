package virtualprinter

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func acceptedName(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".3mf") && len(name) <= 250 && !strings.ContainsAny(name, "\r\n\x00/\\")
}
func (v *instance) ftp(conn net.Conn) {
	rd := bufio.NewReaderSize(conn, 16<<10)
	send := func(code int, s string) bool { _, e := fmt.Fprintf(conn, "%d %s\r\n", code, s); return e == nil }
	if !send(220, "Bambastic FTPS ready") {
		return
	}
	user := ""
	auth := false
	protect := true
	var passive *net.TCPListener
	defer func() {
		if passive != nil {
			passive.Close()
		}
	}()
	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		raw, e := rd.ReadSlice('\n')
		if e != nil {
			return
		}
		verb, arg, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
		cmd := strings.ToUpper(verb)
		switch cmd {
		case "QUIT":
			send(221, "Bye")
			return
		case "USER":
			user = arg
			auth = false
			send(331, "Password required")
			continue
		case "PASS":
			auth = user == "bblp" && subtle.ConstantTimeCompare([]byte(arg), []byte(v.c.Code)) == 1
			if auth {
				send(230, "Logged in")
			} else {
				send(530, "Login failed")
				return
			}
			continue
		case "FEAT":
			fmt.Fprint(conn, "211-Features\r\n EPSV\r\n PASV\r\n UTF8\r\n PBSZ\r\n PROT\r\n211 End\r\n")
			continue
		}
		if !auth {
			send(530, "Login required")
			continue
		}
		switch cmd {
		case "SYST":
			send(215, "UNIX Type: L8")
		case "PWD", "XPWD":
			send(257, `"/"`)
		case "CWD", "CDUP", "MKD":
			send(250, "Ready")
		case "TYPE", "OPTS", "PBSZ", "NOOP", "ALLO":
			send(200, "OK")
		case "PROT":
			protect = !strings.EqualFold(arg, "C")
			send(200, "OK")
		case "PASV", "EPSV":
			if passive != nil {
				passive.Close()
				passive = nil
			}
			ip := net.ParseIP(v.s.listeningIP(v.c))
			base := 50000 + int((v.c.ID-1)%100)*10
			for i := 0; i < 10; i++ {
				passive, e = net.ListenTCP("tcp4", &net.TCPAddr{IP: ip, Port: base + i})
				if e == nil {
					break
				}
			}
			if passive == nil {
				send(425, "No passive port available")
				continue
			}
			port := passive.Addr().(*net.TCPAddr).Port
			if cmd == "EPSV" {
				send(229, fmt.Sprintf("Entering Extended Passive Mode (|||%d|)", port))
			} else {
				send(227, fmt.Sprintf("Entering Passive Mode (%s,%d,%d)", strings.ReplaceAll(ip.String(), ".", ","), port/256, port%256))
			}
		case "STOR", "LIST", "NLST", "MLSD":
			if passive == nil {
				send(425, "Use passive mode")
				continue
			}
			name := filepath.Base(strings.ReplaceAll(arg, "\\", "/"))
			if cmd == "STOR" && !acceptedName(name) {
				send(553, "A 3MF file is required")
				continue
			}
			send(150, "Opening transfer")
			passive.SetDeadline(time.Now().Add(20 * time.Second))
			dc, e := passive.AcceptTCP()
			passive.Close()
			passive = nil
			if e != nil {
				send(425, "Data channel unavailable")
				continue
			}
			clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			if dc.RemoteAddr().(*net.TCPAddr).IP.String() != clientIP {
				dc.Close()
				send(425, "Data peer mismatch")
				continue
			}
			dc.SetDeadline(time.Now().Add(10 * time.Minute))
			var data net.Conn = dc
			if protect {
				data = tls.Server(dc, v.tls)
			}
			if cmd != "STOR" {
				if tlsConn, ok := data.(*tls.Conn); ok {
					_ = tlsConn.HandshakeContext(v.ctx)
				}
				data.Close()
				send(226, "Transfer complete")
				continue
			}
			tmp, e := os.CreateTemp(v.s.dir, "incoming-*.3mf")
			if e != nil {
				data.Close()
				send(451, "Cannot store file")
				continue
			}
			stop := contextAfter(v, dc)
			n, e := io.Copy(tmp, io.LimitReader(data, (1<<30)+1))
			stop()
			data.Close()
			ce := tmp.Close()
			if e != nil || ce != nil || n == 0 || n > 1<<30 {
				os.Remove(tmp.Name())
				send(552, "Upload incomplete or oversized")
				continue
			}
			if e = v.s.stage(v.ctx, v.c, tmp.Name(), name, clientIP); e != nil {
				os.Remove(tmp.Name())
				send(451, "Could not retain upload")
				continue
			}
			if options := v.optionsFor(name); options != nil {
				js, _ := json.Marshal(options)
				v.s.db.ExecContext(v.ctx, "UPDATE bb_vp_received SET options=?,ready_at=? WHERE path=?", string(js), time.Now().Unix(), tmp.Name())
				v.s.changed()
			}
			send(226, "Transfer complete")
		default:
			send(502, "Unsupported command")
		}
	}
}
func contextAfter(v *instance, c net.Conn) func() bool {
	return context.AfterFunc(v.ctx, func() { c.Close() })
}
