package production

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"

	"bambastic/internal/access"
)

type archiveGroup struct {
	files  []row
	reason string
}

func (s *Service) archiveGroups(ctx context.Context, tx *sql.Tx, p access.Principal) ([]archiveGroup, error) {
	files, e := records(ctx, tx, `SELECT a.*, (SELECT COUNT(*) FROM print_log_entries l WHERE l.archive_id=a.id) run_count,
	(a.status='printing' OR julianday(a.completed_at)>julianday('now','-30 minutes') OR EXISTS(SELECT 1 FROM print_log_entries l WHERE l.archive_id=a.id AND (l.status IN ('printing','running','paused') OR julianday(l.completed_at)>julianday('now','-30 minutes'))) OR EXISTS(SELECT 1 FROM print_queue q WHERE q.archive_id=a.id AND q.status IN ('pending','dispatching','printing','awaiting_review')) OR EXISTS(SELECT 1 FROM bb_captures c WHERE c.archive_id=a.id) OR EXISTS(SELECT 1 FROM pipeline_runs r WHERE r.source_archive_id=a.id AND r.status IN ('queued','slicing','dispatching','in_progress')) OR EXISTS(SELECT 1 FROM active_print_spoolman m WHERE m.archive_id=a.id)) busy
	FROM print_archives a WHERE a.deleted_at IS NULL AND length(a.content_hash)>0 AND a.content_hash IN (SELECT content_hash FROM print_archives WHERE deleted_at IS NULL GROUP BY content_hash,created_by_id,project_id HAVING COUNT(*)>1) ORDER BY a.id`)
	if e != nil {
		return nil, e
	}
	type identity struct {
		hash           string
		owner, project int64
	}
	by := map[identity][]row{}
	for _, f := range files {
		if !s.archiveFullScope(ctx, tx, p, f) {
			continue
		}
		k := identity{val(f["content_hash"]), num(f["created_by_id"]), num(f["project_id"])}
		by[k] = append(by[k], f)
	}
	out := []archiveGroup{}
	for _, group := range by {
		if len(group) < 2 {
			continue
		}
		g := archiveGroup{files: group}
		extras := map[string]string{}
		for _, f := range group {
			if flag(f["busy"]) {
				g.reason = "busy"
				break
			}
			for _, k := range []string{"source_3mf_path", "f3d_path", "timelapse_path"} {
				rel := val(f[k])
				if rel == "" {
					continue
				}
				if extras[k] != "" && extras[k] != rel {
					g.reason = "conflicting_files"
				}
				extras[k] = rel
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return num(out[i].files[0]["id"]) < num(out[j].files[0]["id"]) })
	return out, nil
}
func archiveBrief(f row) row {
	return row{"id": f["id"], "print_name": f["print_name"], "filename": f["filename"], "created_at": f["created_at"], "run_count": f["run_count"]}
}
func mergeArchiveGroup(ctx context.Context, tx *sql.Tx, files []row) ([]int64, error) {
	head := files[0]
	survivor := num(head["id"])
	merged := []int64{}
	tags := []string{}
	seen := map[string]bool{}
	notes := []string{}
	noted := map[string]bool{}
	for _, f := range files {
		for _, t := range strings.Split(val(f["tags"]), ",") {
			t = strings.TrimSpace(t)
			key := strings.ToLower(t)
			if t != "" && !seen[key] {
				seen[key] = true
				tags = append(tags, t)
			}
		}
		if n := strings.TrimSpace(val(f["notes"])); n != "" && !noted[n] {
			notes = append(notes, n)
			noted[n] = true
		}
	}
	for _, f := range files[1:] {
		id := num(f["id"])
		for _, ref := range [][2]string{{"print_log_entries", "archive_id"}, {"print_queue", "archive_id"}, {"print_batches", "archive_id"}, {"project_bom_items", "archive_id"}, {"spool_usage_history", "archive_id"}, {"telegram_pending_verdicts", "archive_id"}, {"library_folders", "archive_id"}, {"pending_uploads", "archived_id"}, {"pipeline_runs", "source_archive_id"}, {"archive_aliases", "archive_id"}} {
			if _, e := tx.ExecContext(ctx, "UPDATE "+ref[0]+" SET "+ref[1]+"=? WHERE "+ref[1]+"=?", survivor, id); e != nil {
				return nil, e
			}
		}
		if _, e := tx.ExecContext(ctx, "UPDATE bb_photos SET owner_id=? WHERE kind='archives' AND owner_id=?", survivor, id); e != nil {
			return nil, e
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO archive_aliases(merged_archive_id,archive_id) VALUES(?,?)", id, survivor); e != nil {
			return nil, e
		}
		for _, field := range []string{"source_3mf_path", "f3d_path", "timelapse_path", "thumbnail_path", "designer", "makerworld_url"} {
			if val(head[field]) == "" && val(f[field]) != "" {
				head[field] = f[field]
			}
		}
		head["is_favorite"] = flag(head["is_favorite"]) || flag(f["is_favorite"])
		if _, e := tx.ExecContext(ctx, "UPDATE print_archives SET deleted_at=CURRENT_TIMESTAMP,source_3mf_path=NULL,f3d_path=NULL,photos=NULL,timelapse_path=NULL WHERE id=?", id); e != nil {
			return nil, e
		}
		merged = append(merged, id)
	}
	pics, e := records(ctx, tx, "SELECT filename FROM bb_photos WHERE kind='archives' AND owner_id=? ORDER BY filename", survivor)
	if e != nil {
		return nil, e
	}
	names := []string{}
	for _, p := range pics {
		names = append(names, val(p["filename"]))
	}
	head["photos"] = jstr(names)
	head["notes"] = strings.Join(notes, "\n\n")
	head["tags"] = strings.Join(tags, ",")
	e = update(ctx, tx, "print_archives", survivor, head, []string{"source_3mf_path", "f3d_path", "timelapse_path", "thumbnail_path", "designer", "makerworld_url", "is_favorite", "notes", "tags", "photos"})
	return merged, e
}
func (s *Service) mergeArchives(mode string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		p := access.From(r.Context())
		if !p.Has("archives:update_all") || !p.Has("archives:delete_all") {
			return nil, fail(403, "Archive update and delete permissions required")
		}
		var ids []int64
		if mode == "merge" {
			body, e := input(w, r)
			if e != nil {
				return nil, e
			}
			ids, e = idsOf(body["survivor_ids"])
			if e != nil || len(ids) < 1 || len(ids) > 500 {
				return nil, fail(422, "Choose up to 500 duplicate groups")
			}
		}
		ctx := r.Context()
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		groups, e := s.archiveGroups(ctx, tx, p)
		if e != nil {
			return nil, e
		}
		if mode == "merge-count" {
			return row{"groups": len(groups)}, nil
		}
		if mode == "merge-preview" {
			out := []row{}
			for _, g := range groups {
				other := []row{}
				for _, f := range g.files[1:] {
					other = append(other, archiveBrief(f))
				}
				out = append(out, row{"survivor": archiveBrief(g.files[0]), "merged": other, "skipped": nullableString(g.reason)})
			}
			return row{"groups": out}, nil
		}
		index := map[int64]archiveGroup{}
		for _, g := range groups {
			index[num(g.files[0]["id"])] = g
		}
		done, skips := []row{}, []row{}
		for _, id := range ids {
			g, ok := index[id]
			reason := g.reason
			if !ok {
				reason = "not_a_group"
			}
			if reason != "" {
				skips = append(skips, row{"survivor_id": id, "reason": reason})
				continue
			}
			merged, e := mergeArchiveGroup(ctx, tx, g.files)
			if e != nil {
				return nil, e
			}
			done = append(done, row{"survivor_id": id, "merged_ids": merged})
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		for _, m := range done {
			s.emit(row{"type": "archive_updated", "data": row{"id": m["survivor_id"]}})
		}
		return row{"merged": done, "skipped": skips}, nil
	}
}
