package production

import (
	"bambastic/internal/access"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Each file transfer leaves a local intent and an SQL commit marker. If the
// process dies between operations, reopening the database restores the source
// or finalizes the destination according to the original transaction outcome.
type externalIntent struct {
	ID, Source, SourceName, Destination, DestinationName, PublishedIdentity, Hash string
}
type externalEdits struct{ intents []externalIntent }

func (s *Service) externalJournal(name string) string {
	return filepath.Join(s.dir, "production", "external-work", name)
}
func (s *Service) writeIntent(i externalIntent) error {
	p := s.externalJournal(i.ID + ".json")
	f, e := os.CreateTemp(filepath.Dir(p), "write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	e = json.NewEncoder(f).Encode(i)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), p); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(p))
}
func syncDirectory(path string) error {
	d, e := os.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func linkMove(root *os.Root, from, to string) error {
	if !validFileName(from) || !validFileName(to) {
		return os.ErrInvalid
	}
	if err := root.Link(from, to); err != nil {
		return err
	}
	return root.Remove(from)
}
func (s *Service) openMount(raw string) (*os.Root, error) {
	target, e := s.externalRoot(raw)
	if e != nil {
		return nil, e
	}
	roots := os.Getenv("BAMBASTIC_EXTERNAL_ROOTS")
	if roots == "" {
		roots = os.Getenv("BAMBUDDY_EXTERNAL_ROOTS")
	}
	for _, allowed := range filepath.SplitList(roots) {
		if !filepath.IsAbs(allowed) {
			continue
		}
		base, e := filepath.EvalSymlinks(allowed)
		if e != nil || !within(base, target) {
			continue
		}
		dir, e := os.OpenRoot(base)
		if e != nil {
			continue
		}
		rel, e := filepath.Rel(base, target)
		if e != nil {
			dir.Close()
			return nil, e
		}
		root, e := dir.OpenRoot(rel)
		dir.Close()
		return root, e
	}
	return nil, fail(400, "External path is outside configured roots")
}
func regularHash(root *os.Root, name string) (string, error) {
	st, e := root.Lstat(name)
	if e != nil {
		return "", e
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<30 {
		return "", errors.New("source must be a regular library file")
	}
	f, e := root.Open(name)
	if e != nil {
		return "", e
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(st, actual) {
		return "", errors.New("file changed during transfer")
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, (1<<30)+1))
	if e != nil {
		return "", e
	}
	if n != actual.Size() {
		return "", errors.New("file changed during transfer")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Service) resolveIntent(i externalIntent, committed bool) error {
	prefix := ".bambastic-" + i.ID
	if i.Source != "" {
		root, e := s.openMount(i.Source)
		if e != nil {
			return e
		}
		defer root.Close()
		stage := prefix + "-source"
		_, e = root.Lstat(stage)
		if e == nil {
			if committed {
				sum, e := regularHash(root, stage)
				if e != nil || sum != i.Hash {
					return errors.New("source changed during transfer; retained for recovery")
				}
				e = root.Remove(stage)
				if e != nil {
					return e
				}
			} else {
				e = renameExternal(root, stage, i.SourceName)
				if errors.Is(e, os.ErrExist) {
					a, e1 := root.Lstat(stage)
					b, e2 := root.Lstat(i.SourceName)
					if e1 == nil && e2 == nil && os.SameFile(a, b) {
						e = root.Remove(stage)
					}
				}
				if e != nil {
					return e
				}
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if i.Destination != "" {
		root, e := s.openMount(i.Destination)
		if e != nil {
			return e
		}
		defer root.Close()
		stage := prefix + "-dest"
		if !committed && i.PublishedIdentity != "" {
			info, e := root.Lstat(i.DestinationName)
			if e == nil && externalIdentity(info) == i.PublishedIdentity {
				sum, e := regularHash(root, i.DestinationName)
				if e != nil {
					return e
				}
				if sum == i.Hash {
					// Quarantine before unlinking so a concurrent external replacement is
					// checked again under a private name.
					cleanup := prefix + "-undo"
					if e = renameExternal(root, i.DestinationName, cleanup); e != nil {
						return e
					}
				}
			} else if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			undo := prefix + "-undo"
			if st, e := root.Lstat(undo); e == nil {
				hash, e := regularHash(root, undo)
				if e != nil {
					return e
				}
				if externalIdentity(st) == i.PublishedIdentity && hash == i.Hash {
					e = root.Remove(undo)
				} else {
					e = renameExternal(root, undo, i.DestinationName)
				}
				if e != nil {
					return e
				}
			} else if !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
		if e = root.Remove(stage); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if e := os.Remove(s.externalJournal(i.ID + ".json")); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return nil
}
func (s *Service) finishExternal(i externalIntent) {
	var committed bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM bb_external_commits WHERE id=?)", i.ID).Scan(&committed)
	if e == nil {
		e = s.resolveIntent(i, committed)
	}
	if e == nil && committed {
		_, e = s.db.ExecContext(ctx, "DELETE FROM bb_external_commits WHERE id=?", i.ID)
	}
	s.mu.Lock()
	if e == nil {
		delete(s.externalPending, i.ID)
	} else {
		if s.externalPending == nil {
			s.externalPending = map[string]externalIntent{}
		}
		s.externalPending[i.ID] = i
	}
	s.mu.Unlock()
	if e != nil {
		log.Printf("external library transfer %s awaiting recovery: %v", i.ID, e)
	}
}
func (s *Service) retryExternal() {
	s.mu.Lock()
	pending := s.externalPending
	s.externalPending = nil
	s.mu.Unlock()
	for _, i := range pending {
		s.finishExternal(i)
	}
}
func (b *externalEdits) finish(s *Service) {
	for i := len(b.intents) - 1; i >= 0; i-- {
		s.finishExternal(b.intents[i])
	}
}
func (s *Service) recoverExternal() error {
	root := s.externalJournal("")
	if e := os.MkdirAll(root, 0700); e != nil {
		return e
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, e := os.ReadFile(filepath.Join(root, entry.Name()))
		if e != nil {
			return e
		}
		var i externalIntent
		if json.Unmarshal(data, &i) != nil || len(i.ID) != 32 || i.ID+".json" != entry.Name() {
			return fmt.Errorf("invalid external library transfer journal %s", entry.Name())
		}
		s.finishExternal(i)
	}
	return nil
}
func (s *Service) externalFolder(ctx context.Context, tx *sql.Tx, id int64) (row, error) {
	if id == 0 {
		return nil, nil
	}
	r, e := one(ctx, tx, "SELECT * FROM library_folders WHERE id=?", id)
	if e != nil || !flag(r["is_external"]) {
		return nil, e
	}
	return r, nil
}

// transfer stages and verifies the original or target before recording a commit
// marker. Source files are retired only once their immutable copy is durable.
func (b *externalEdits) transfer(ctx context.Context, s *Service, tx *sql.Tx, v row, source, dest row, newName string, remove bool) (err error) {
	if source == nil && dest == nil {
		return nil
	}
	if !validFileName(newName) || strings.HasPrefix(newName, ".bambastic-") || source != nil && !validFileName(val(v["filename"])) {
		return fail(422, "Invalid external filename")
	}
	if source != nil && remove && flag(source["external_readonly"]) {
		return fail(403, "Source external folder is read-only")
	}
	if dest != nil && flag(dest["external_readonly"]) {
		return fail(403, "Destination external folder is read-only")
	}
	intent := externalIntent{ID: uuid(), Hash: val(v["file_hash"])}
	if intent.Hash == "" {
		return fail(409, "Rescan the source before transferring")
	}
	var src, dst *os.Root
	var e error
	if source != nil {
		src, e = s.openMount(val(source["external_path"]))
		if e != nil {
			return e
		}
		defer src.Close()
		name := val(v["filename"])
		hash, e := regularHash(src, name)
		if errors.Is(e, os.ErrNotExist) {
			return fail(404, "Source file missing on disk")
		}
		if e != nil || hash != intent.Hash {
			return fail(409, "Source changed on disk; rescan the folder")
		}
		if remove {
			intent.Source = val(source["external_path"])
			intent.SourceName = name
		}
	}
	if dest != nil {
		dst, e = s.openMount(val(dest["external_path"]))
		if e != nil {
			return e
		}
		defer dst.Close()
		intent.Destination = val(dest["external_path"])
		intent.DestinationName = newName
		if _, e = dst.Lstat(newName); e == nil {
			return fail(409, "Filename already exists in the external folder")
		} else if !errors.Is(e, os.ErrNotExist) {
			return fail(400, "External destination is inaccessible")
		}
	}
	if source != nil && dst == nil {
		// A volume may have been remounted since the last scan. Ensure its current
		// bytes are actually present in the internal store before retiring it.
		file, e := src.Open(val(v["filename"]))
		if e != nil {
			return fail(404, "Source file missing on disk")
		}
		cached, e := s.prepareAsset(io.LimitReader(file, (1<<30)+1), "library", val(v["filename"]))
		file.Close()
		if e != nil {
			return e
		}
		if val(cached["file_hash"]) != intent.Hash {
			return fail(409, "Source changed on disk; rescan the folder")
		}
		if cached["file_path"] != v["file_path"] {
			return fail(409, "A refreshed source snapshot is required")
		}
		if e = syncDirectory(filepath.Join(s.dir, "production", "objects")); e != nil {
			return e
		}
	}
	if intent.Source == "" && intent.Destination == "" {
		return nil
	}
	if err = s.writeIntent(intent); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if e := s.resolveIntent(intent, false); e != nil {
				log.Printf("external transfer rollback %s: %v", intent.ID, e)
			}
		}
	}()
	if dst != nil {
		path, e := s.assetPath(v["file_path"])
		if e != nil {
			return e
		}
		f, e := os.Open(path)
		if e != nil {
			return fail(404, "Source snapshot unavailable")
		}
		defer f.Close()
		stage := ".bambastic-" + intent.ID + "-dest"
		w, e := dst.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0660)
		if e != nil {
			return fail(400, "Cannot write to external folder")
		}
		h := sha256.New()
		_, e = io.Copy(io.MultiWriter(w, h), f)
		if e == nil {
			e = w.Sync()
		}
		st, se := w.Stat()
		ce := w.Close()
		if e != nil || se != nil || ce != nil || hex.EncodeToString(h.Sum(nil)) != intent.Hash {
			return fail(409, "Unable to verify the copied file")
		}
		intent.PublishedIdentity = externalIdentity(st)
		if e = s.writeIntent(intent); e != nil {
			return e
		}
		if e = renameExternal(dst, stage, newName); e != nil {
			if errors.Is(e, os.ErrExist) {
				return fail(409, "Filename already exists in the external folder")
			}
			return fail(400, "Cannot publish the external file")
		}
		v["fs_modified_at"] = st.ModTime().UTC().Format(time.RFC3339Nano)
	}
	if src != nil && remove {
		stage := ".bambastic-" + intent.ID + "-source"
		if e = renameExternal(src, intent.SourceName, stage); e != nil {
			return fail(409, "Cannot move the external source")
		}
		hash, e := regularHash(src, stage)
		if e != nil || hash != intent.Hash {
			return fail(409, "Source changed on disk; rescan the folder")
		}
	}
	for _, r := range []*os.Root{src, dst} {
		if r != nil {
			d, e := r.Open(".")
			if e != nil {
				return e
			}
			e = d.Sync()
			d.Close()
			if e != nil {
				return e
			}
		}
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO bb_external_commits(id) VALUES(?)", intent.ID); e != nil {
		return e
	}
	b.intents = append(b.intents, intent)
	return nil
}
func (b *externalEdits) insertLibrary(ctx context.Context, s *Service, tx *sql.Tx, v row, folder int64) (int64, error) {
	target, e := s.externalFolder(ctx, tx, folder)
	if e != nil {
		return 0, e
	}
	if target != nil {
		if e = b.transfer(ctx, s, tx, v, nil, target, val(v["filename"]), false); e != nil {
			return 0, e
		}
		v["is_external"] = true
	}
	return insert(ctx, tx, "library_files", v)
}
func (s *Service) forgetExternal(ctx context.Context, tx *sql.Tx, k int64) error {
	if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bb_reclaim(path) SELECT file_path FROM library_files WHERE id=? UNION SELECT path FROM bb_photos WHERE owner_id=? AND kind='library'", k, k); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "DELETE FROM bb_photos WHERE owner_id=? AND kind='library'", k); e != nil {
		return e
	}
	// Preserve queue history; a user may re-index the external file later.
	if _, e := tx.ExecContext(ctx, "UPDATE print_queue SET library_file_id=NULL WHERE library_file_id=?", k); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, "DELETE FROM library_files WHERE id=? AND is_external", k)
	return e
}
func transferCode(err error) string {
	if errors.Is(err, sql.ErrNoRows) {
		return "not_owner"
	}
	switch msg := strings.ToLower(err.Error()); {
	case strings.Contains(msg, "already exists"):
		return "name_collision"
	case strings.Contains(msg, "source external folder is read-only"):
		return "source_readonly"
	case strings.Contains(msg, "source changed"):
		return "source_changed"
	case strings.Contains(msg, "missing"), strings.Contains(msg, "unavailable"):
		return "source_missing"
	default:
		return "transfer_failed"
	}
}

// SaveLibrarySlice stores validated slicer output with the same immutable bytes
// used by upload and dispatch. A read-only or offline volume retains a managed
// copy and reports the fallback reason with the slice result.
func (s *Service) SaveLibrarySlice(ctx context.Context, p access.Principal, f io.Reader, name string, folder, project int64, meta map[string]any) (int64, string, string, error) {
	if !p.Has("library:upload") {
		return 0, "", "", fail(403, "Library upload permission required")
	}
	if p.ID != 0 || p.KeyID != 0 {
		var e error
		p, e = s.durableActor(ctx, p.ID, p.KeyID)
		if e != nil || !p.Has("library:upload") {
			return 0, "", "", fail(403, "Slice owner no longer has upload permission")
		}
	}
	if !validFileName(name) {
		return 0, "", "", fail(400, "Invalid slice filename")
	}
	v, e := s.prepareAsset(f, "library", name)
	if e != nil {
		return 0, "", "", e
	}
	var info map[string]any
	if e = json.Unmarshal([]byte(val(v["file_metadata"])), &info); e != nil {
		return 0, "", "", e
	}
	for k, x := range meta {
		info[k] = x
	}
	v["source_type"] = "sliced"
	v["file_type"] = "gcode.3mf"
	v["project_id"] = id(project)
	v["created_by_id"] = id(p.ID)
	v["folder_id"] = id(folder)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return 0, "", "", e
	}
	var edits externalEdits
	defer edits.finish(s)
	defer tx.Rollback()
	dest, e := s.externalFolder(ctx, tx, folder)
	if errors.Is(e, sql.ErrNoRows) {
		dest = nil
		v["folder_id"] = nil
	} else if e != nil {
		return 0, "", "", e
	}
	fallback := ""
	if dest != nil {
		switch {
		case flag(dest["external_readonly"]):
			fallback = "external_readonly"
		case val(dest["external_path"]) == "":
			fallback = "external_no_path"
		case !folderWrite(p, dest):
			fallback = "external_not_writable"
			v["folder_id"] = nil
		default:
			root, e := s.openMount(val(dest["external_path"]))
			if e != nil {
				fallback = "external_unreachable"
				break
			}
			defer root.Close()
			candidate := name
			stem := strings.TrimSuffix(name, ".gcode.3mf")
			fallback = "external_name_collision"
			for n := 1; n <= 1000; n++ {
				if n > 1 {
					candidate = fmt.Sprintf("%s (%d).gcode.3mf", stem, n)
				}
				if !validFileName(candidate) {
					fallback = "external_invalid_name"
					break
				}
				if _, e := root.Lstat(candidate); e == nil {
					continue
				} else if !errors.Is(e, os.ErrNotExist) {
					fallback = "external_unreachable"
					break
				}
				if e = edits.transfer(ctx, s, tx, v, nil, dest, candidate, false); e != nil {
					if transferCode(e) == "name_collision" {
						continue
					}
					var problem failure
					if !errors.As(e, &problem) {
						return 0, "", "", e
					}
					fallback = "external_not_writable"
					break
				}
				name = candidate
				v["filename"] = name
				v["is_external"] = true
				fallback = ""
				break
			}
		}
	}
	if fallback != "" {
		info["external_write_fallback"] = fallback
	}
	v["file_metadata"] = jstr(info)
	k, e := insert(ctx, tx, "library_files", v)
	if e != nil {
		return 0, "", "", e
	}
	return k, name, fallback, tx.Commit()
}
