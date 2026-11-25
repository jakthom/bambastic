package production

import (
	"bambastic/internal/access"
	"context"
	"io"
	"strings"
)

// ImportLibrary admits a fetched provider asset, preserving provenance and
// resolving a retry to the visible file already imported for that destination.
func (s *Service) ImportLibrary(ctx context.Context, p access.Principal, content io.Reader, name string, folder int64, provider, sourceURL string) (int64, bool, error) {
	if !p.Has("library:upload") {
		return 0, false, fail(403, "Library upload permission required")
	}
	if !validFileName(name) || sourceURL == "" || len(sourceURL) > 512 || strings.TrimSpace(provider) == "" || len(provider) > 32 {
		return 0, false, fail(422, "Invalid import filename or provider identity")
	}
	if e := s.checkLibraryDestination(ctx, s.db, p, folder, true); e != nil {
		return 0, false, e
	}
	v, e := s.prepareAsset(io.LimitReader(content, (1<<30)+1), "library", name)
	if e != nil {
		return 0, false, e
	}
	if num(v["file_size"]) > 1<<30 {
		return 0, false, fail(413, "File too large")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return 0, false, e
	}
	var edits externalEdits
	defer edits.finish(s)
	defer tx.Rollback()
	if e = s.checkLibraryDestination(ctx, tx, p, folder, true); e != nil {
		return 0, false, e
	}
	rs, e := records(ctx, tx, "SELECT * FROM library_files WHERE source_type=? AND source_url=? AND coalesce(folder_id,0)=? AND deleted_at IS NULL ORDER BY id", provider, sourceURL, folder)
	if e != nil {
		return 0, false, e
	}
	for _, x := range rs {
		if s.accessible(ctx, tx, p, "library", x, "read") {
			return num(x["id"]), true, nil
		}
	}
	v["folder_id"] = id(folder)
	v["created_by_id"] = id(p.ID)
	v["source_type"] = provider
	v["source_url"] = sourceURL
	k, e := edits.insertLibrary(ctx, s, tx, v, folder)
	if e != nil {
		return 0, false, e
	}
	return k, false, tx.Commit()
}
