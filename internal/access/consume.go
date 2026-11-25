package access

import "context"

// Consume atomically retires a previously authenticated single-use credential.
func (s *Service) Consume(ctx context.Context, token, kind string) bool {
	result, err := s.DB.ExecContext(ctx, "DELETE FROM bambastic_credentials WHERE hash=? AND kind=? AND expires>unixepoch()", digest(token), kind)
	if err != nil {
		return false
	}
	count, err := result.RowsAffected()
	return err == nil && count == 1
}
