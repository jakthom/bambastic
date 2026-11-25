package production

import (
	"bambastic/internal/access"
	"context"
	"database/sql"
)

// MediaAsset applies the same scope, ownership, archive alias and soft-delete
// rules used by ordinary library and archive operations.
func MediaAsset(ctx context.Context, db *sql.DB, p access.Principal, kind string, id int64, action string) (map[string]any, error) {
	if kind != "archives" && kind != "library" {
		return nil, sql.ErrNoRows
	}
	s := Service{db: db}
	return s.getAsset(ctx, db, p, kind, id, action)
}

// SetFilamentPreview attaches the slicer for unsliced model-only projects.
func (s *Service) SetFilamentPreview(f func(context.Context, access.Principal, string, int64, int, string, bool) ([]map[string]any, error)) {
	s.preview = f
}
