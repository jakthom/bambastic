package production

import (
	"bambastic/internal/access"
	"context"
	"database/sql"
	"io"
	"os"
)

func (s *Service) ImportProjectFile(ctx context.Context, tx *sql.Tx, p access.Principal, source io.Reader, name string, folder int64) (int64, error) {
	if !p.Has("library:upload") || !validFileName(name) {
		return 0, fail(403, "Library upload denied")
	}
	if err := s.targetFolder(ctx, tx, p, folder); err != nil {
		return 0, err
	}
	return s.storeAsset(ctx, tx, p, source, "library", name, folder, 0, 0)
}
func (s *Service) OpenProjectFile(ctx context.Context, tx *sql.Tx, p access.Principal, id int64) (io.ReadCloser, error) {
	record, e := s.getAsset(ctx, tx, p, "library", id, "read")
	if e != nil {
		return nil, e
	}
	name, e := s.assetPath(record["file_path"])
	if e != nil {
		return nil, e
	}
	return os.Open(name)
}
