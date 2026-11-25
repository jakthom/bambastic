package production

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

type deadlineConn struct {
	net.Conn
	stop func() bool
}

func (c *deadlineConn) Close() error { c.stop(); return c.Conn.Close() }

func (l *lan) files(ctx context.Context) (*ftp.ServerConn, error) {
	cfg := DeviceTLS(l.fingerprint)
	cfg.MaxVersion = tls.VersionTLS12
	host := val(l.printer["ip_address"])
	d := net.Dialer{Timeout: 8 * time.Second}
	dial := func(network, addr string) (net.Conn, error) {
		// A passive data connection belongs to the enrolled printer. Some firmware
		// advertises an unusable address, so retain its port and use the control IP.
		_, port, e := net.SplitHostPort(addr)
		if e != nil {
			return nil, e
		}
		raw, e := d.DialContext(ctx, network, net.JoinHostPort(host, port))
		if e != nil {
			return nil, e
		}
		if until, ok := ctx.Deadline(); ok {
			raw.SetDeadline(until)
		}
		stop := context.AfterFunc(ctx, func() { raw.Close() })
		return &deadlineConn{tls.Client(raw, cfg), stop}, nil
	}
	c, e := ftp.Dial(net.JoinHostPort(host, "990"), ftp.DialWithTLS(cfg), ftp.DialWithDialFunc(dial), ftp.DialWithShutTimeout(8*time.Second))
	if e != nil {
		return nil, e
	}
	if e = c.Login("bblp", val(l.printer["access_code"])); e != nil {
		c.Quit()
		return nil, e
	}
	return c, nil
}

type fileRetriever interface {
	Retrieve(context.Context, string, string) (string, error)
}

func (l *lan) Retrieve(ctx context.Context, remote, temp string) (string, error) {
	name := filepath.Base(strings.ReplaceAll(remote, "\\", "/"))
	if !validFileName(name) {
		return "", errors.New("missing file name")
	}
	names := []string{name}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".3mf" && ext != ".gcode" {
		names = []string{name + ".gcode.3mf", name + ".3mf"}
	}
	c, e := l.files(ctx)
	if e != nil {
		return "", e
	}
	defer c.Quit()
	for _, f := range names {
		for _, folder := range []string{"/", "/cache", "/model"} {
			resp, e := c.Retr(filepath.ToSlash(filepath.Join(folder, f)))
			if e != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				continue
			}
			out, e := os.OpenFile(temp, os.O_TRUNC|os.O_WRONLY, 0600)
			if e != nil {
				resp.Close()
				return "", e
			}
			n, e := io.Copy(out, io.LimitReader(resp, (1<<30)+1))
			ce := out.Close()
			re := resp.Close()
			if e != nil {
				return "", e
			}
			if ce != nil {
				return "", ce
			}
			if re != nil {
				return "", re
			}
			if n == 0 || n > 1<<30 {
				return "", errors.New("printer file is empty or oversized")
			}
			return f, nil
		}
	}
	return "", errors.New("printer file has not appeared in storage")
}
func plateDigest(path, name, digest string) string {
	if strings.EqualFold(filepath.Ext(name), ".gcode") {
		return digest
	}
	if !strings.EqualFold(filepath.Ext(name), ".3mf") {
		return ""
	}
	z, e := zip.OpenReader(path)
	if e != nil {
		return ""
	}
	defer z.Close()
	type member struct {
		f *zip.File
		n int64
	}
	items := []member{}
	var bytes uint64
	for _, f := range z.File {
		m := plateEntry.FindStringSubmatch(f.Name)
		if len(m) != 3 || m[2] != "gcode" {
			continue
		}
		if f.UncompressedSize64 > 1<<30 || bytes+f.UncompressedSize64 > 1<<30 {
			return ""
		}
		bytes += f.UncompressedSize64
		items = append(items, member{f, num(m[1])})
	}
	if len(items) == 0 {
		return ""
	}
	sort.Slice(items, func(i, j int) bool { return items[i].n < items[j].n })
	hash := sha256.New()
	for _, it := range items {
		fmt.Fprintf(hash, "%s\x00%d\x00", it.f.Name, it.f.UncompressedSize64)
		rd, e := it.f.Open()
		if e != nil {
			return ""
		}
		n, e := io.Copy(hash, io.LimitReader(rd, int64(it.f.UncompressedSize64)+1))
		rd.Close()
		if e != nil || n != int64(it.f.UncompressedSize64) {
			return ""
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *Service) captureLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		work, e := records(ctx, s.db, "SELECT * FROM bb_captures WHERE due<=unixepoch() AND tries<5 ORDER BY due,run_id LIMIT 10")
		if e != nil {
			continue
		}
		for _, j := range work {
			if ctx.Err() != nil {
				return
			}
			s.capture(ctx, j)
		}
	}
}
func (s *Service) capture(ctx context.Context, j row) {
	s.mu.RLock()
	d := s.devices[num(j["printer_id"])]
	var h fileRetriever
	if d != nil && d.h != nil && d.h.Online() {
		h, _ = d.h.(fileRetriever)
	}
	s.mu.RUnlock()
	if h == nil {
		return
	}
	temp, e := os.CreateTemp(filepath.Join(s.dir, "production"), "receive-*")
	if e != nil {
		return
	}
	temp.Close()
	defer os.Remove(temp.Name())
	transfer, cancel := context.WithTimeout(ctx, 5*time.Minute)
	name, e := h.Retrieve(transfer, val(j["remote_name"]), temp.Name())
	cancel()
	if e != nil {
		if ctx.Err() == nil {
			s.db.ExecContext(ctx, "UPDATE bb_captures SET tries=tries+1,due=unixepoch()+min(600,10*(1<<tries)),last_error=? WHERE run_id=?", e.Error(), j["run_id"])
		}
		return
	}
	f, e := os.Open(temp.Name())
	if e != nil {
		return
	}
	defer f.Close()
	v, e := s.prepareAsset(f, "archives", name)
	if e != nil {
		s.db.ExecContext(ctx, "UPDATE bb_captures SET tries=5,last_error='Unusable printer file' WHERE run_id=?", j["run_id"])
		return
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return
	}
	defer tx.Rollback()
	a, e := one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", j["archive_id"])
	if e != nil {
		return
	}
	if a["deleted_at"] == nil && val(a["file_path"]) == "" {
		// Resends of the same sliced plates may arrive in a different ZIP wrapper.
		// Reattach only unowned, unlinked files with no competing open run or queued work.
		existing, err := one(ctx, tx, `SELECT a.id FROM print_archives a WHERE a.deleted_at IS NULL AND a.gcode_hash=? AND a.gcode_hash!='' AND a.created_by_id IS NULL AND a.project_id IS NULL AND a.library_file_id IS NULL AND a.id!=? AND NOT EXISTS(SELECT 1 FROM print_log_entries l WHERE l.archive_id=a.id AND l.completed_at IS NULL) AND NOT EXISTS(SELECT 1 FROM print_queue q WHERE q.archive_id=a.id AND q.status IN ('pending','dispatching','printing')) ORDER BY a.id LIMIT 1`, v["gcode_hash"], a["id"])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return
		}
		aid := num(a["id"])
		if existing != nil {
			aid = num(existing["id"])
			if _, e = tx.ExecContext(ctx, "UPDATE print_log_entries SET archive_id=? WHERE id=? AND archive_id=?", aid, j["run_id"], a["id"]); e != nil {
				return
			}
			if _, e = tx.ExecContext(ctx, "INSERT OR REPLACE INTO archive_aliases(merged_archive_id,archive_id) VALUES(?,?)", a["id"], aid); e != nil {
				return
			}
			if _, e = tx.ExecContext(ctx, "UPDATE print_archives SET deleted_at=CURRENT_TIMESTAMP WHERE id=?", a["id"]); e != nil {
				return
			}
			if e = update(ctx, tx, "print_archives", aid, a, []string{"status", "printer_id", "billing_run_id", "started_at", "completed_at", "plate_id"}); e != nil {
				return
			}
		}
		fields := []string{"filename", "file_path", "file_size", "content_hash", "gcode_hash", "extra_data", "plate_estimates", "print_time_seconds", "filament_used_grams", "sliced_for_model"}
		if e = update(ctx, tx, "print_archives", aid, v, fields); e != nil {
			return
		}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM bb_captures WHERE run_id=?", j["run_id"]); e != nil {
		return
	}
	_ = tx.Commit()
}
