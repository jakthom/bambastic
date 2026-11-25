package management

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"bambastic/internal/lanhttp"
	"golang.org/x/crypto/ssh"
)

func stationImage(path string) (*os.File, string, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, "", errors.New("Station binary unavailable")
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 128<<20 {
		return nil, "", errors.New("Invalid station binary")
	}
	img, e := elf.NewFile(f)
	if e != nil || img.Machine != elf.EM_AARCH64 && img.Machine != elf.EM_ARM {
		return nil, "", errors.New("Station update requires a Linux ARM executable")
	}
	hash := sha256.New()
	if _, e = io.Copy(hash, f); e != nil {
		return nil, "", e
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return nil, "", e
	}
	ok = true
	return f, hex.EncodeToString(hash.Sum(nil)), nil
}
func deliverStationBinary(ctx context.Context, addr string, signer ssh.Signer, pin string, file io.Reader, digest string, trust func(string) error) error {
	if b, e := hex.DecodeString(digest); e != nil || len(b) != 32 {
		return errors.New("Invalid station checksum")
	}
	netConn, e := lanhttp.DialContext(ctx, "tcp", addr)
	if e != nil {
		return errors.New("SSH station unavailable")
	}
	defer netConn.Close()
	deadline, _ := ctx.Deadline()
	if !deadline.IsZero() {
		netConn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { netConn.Close() })
	defer stop()
	var observed string
	config := &ssh.ClientConfig{User: "spoolbuddy", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		observed = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if pin != "" && pin != observed {
			return errors.New("Station SSH host key changed")
		}
		return nil
	}}
	conn, chans, reqs, e := ssh.NewClientConn(netConn, addr, config)
	if e != nil {
		return errors.New("SSH identity or pairing check failed")
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()
	if pin == "" {
		if e = trust(observed); e != nil {
			return e
		}
	}
	session, e := client.NewSession()
	if e != nil {
		return errors.New("Cannot open station update session")
	}
	defer session.Close()
	session.Stdin = file
	// The paired, unprivileged account owns its executable. Only the fixed
	// service restart can request elevated privileges. The sole interpolated
	// value is the server-computed hex digest, never a device or API argument.
	run := `set -eu
umask 077
mkdir -p "$HOME/.local/bin"
tmp=$(mktemp "$HOME/.local/bin/.spoolbuddy.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
printf '%s  %s\n' '` + digest + `' "$tmp" | sha256sum -c - >/dev/null
chmod 0755 "$tmp"
mv -f "$tmp" "$HOME/.local/bin/spoolbuddy"
/usr/bin/sudo -n /usr/bin/systemctl restart spoolbuddy.service`
	if e = session.Run(run); e != nil {
		return errors.New("Station installation or service restart failed")
	}
	return nil
}
func (s *Service) deliverStationUpdates(ctx context.Context) error {
	for ctx.Err() == nil {
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		dev, e := one(tx, "SELECT * FROM spoolbuddy_devices WHERE update_status='pending' ORDER BY id LIMIT 1")
		if errors.Is(e, sql.ErrNoRows) {
			tx.Rollback()
			return nil
		}
		if e != nil {
			tx.Rollback()
			return e
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", "/", nil)
		signer, keyErr := s.stationSigningKey(request{Request: req, tx: tx})
		_, e = tx.Exec("UPDATE spoolbuddy_devices SET update_status='updating',update_message='Connecting to paired station' WHERE id=? AND update_status='pending'", dev["id"])
		if e == nil {
			e = tx.Commit()
		} else {
			tx.Rollback()
		}
		if e != nil {
			return e
		}
		e = keyErr
		if e == nil {
			file, sum, err := stationImage(s.stationBinary())
			e = err
			if e == nil {
				deadline, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				ip := net.ParseIP(str(dev["ip_address"]))
				if ip == nil {
					e = errors.New("Invalid station address")
				} else {
					e = deliverStationBinary(deadline, net.JoinHostPort(ip.String(), "22"), signer, str(dev["ssh_host_key"]), file, sum, func(key string) error {
						res, e := s.db.ExecContext(deadline, "UPDATE spoolbuddy_devices SET ssh_host_key=? WHERE id=? AND (ssh_host_key IS NULL OR ssh_host_key='' OR ssh_host_key=?)", key, dev["id"], key)
						if e != nil {
							return e
						}
						n, _ := res.RowsAffected()
						if n != 1 {
							return errors.New("Station pairing changed")
						}
						return nil
					})
				}
				file.Close()
				cancel()
			}
		}
		state, msg := "success", "Station executable installed and service restarted"
		if e != nil {
			state, msg = "failed", e.Error()
		}
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_, err := s.db.ExecContext(done, "UPDATE spoolbuddy_devices SET update_status=?,update_message=? WHERE id=?", state, msg, dev["id"])
		cancel()
		if err != nil {
			return err
		}
		if s.emit != nil {
			s.emit(row{"type": "spoolbuddy_update", "device_id": dev["device_id"], "update_status": state, "update_message": msg})
		}
	}
	return fmt.Errorf("station update worker: %w", ctx.Err())
}
