package cloudbackup

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"bambastic/internal/access"
)

//go:embed release.json
var releaseJSON []byte

// These are the public fields in the release's portable snapshot format. SQL names
// below are selected from this manifest and never taken from a remote file.
var release struct {
	Fields   map[string][]string `json:"fields"`
	Settings map[string]obj      `json:"settings"`
}

func init() {
	if err := json.Unmarshal(releaseJSON, &release); err != nil {
		panic(err)
	}
}

type dataset struct{ category, table, path, key string }

var datasets = []dataset{
	{"spools", "spool", "spools/inventory.json", "spools"},
	{"archives", "print_archives", "archives/print_history.json", "archives"},
	{"archives", "print_log_entries", "archives/print_runs.json", "runs"},
	{"spools", "spool_usage_history", "spools/usage_history.json", "usage_history"},
	{"archives", "archive_aliases", "archives/aliases.json", "aliases"},
}

const settingsPath = "settings/app_settings.json"
const metadataPath = "backup_metadata.json"
const mask = "********"

var urlLogin = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/\s?#]+@`)
var urlSecret = regexp.MustCompile(`(?i)[?&](token|key|signature|auth|access_token|password|x-amz-signature)=[^&\s]+`)

func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, part := range strings.Fields("token secret password passwd access_code api_key apikey passphrase verifier authorization private_key bearer") {
		if strings.Contains(k, part) {
			return true
		}
	}
	return false
}
func secretURL(s string) bool { return urlLogin.MatchString(s) || urlSecret.MatchString(s) }
func filterPreset(v any) any {
	switch a := v.(type) {
	case obj:
		m := obj{}
		for k, x := range a {
			lk := strings.ToLower(k)
			switch {
			case lk == "user_id":
				continue
			case secretKey(k) || lk == "printhost_user" || lk == "printhost_client_cert" || lk == "printhost_ssl_cert":
				if str(x) != "" {
					m[k] = mask
				} else {
					m[k] = x
				}
			default:
				m[k] = filterPreset(x)
			}
		}
		return m
	case []any:
		b := make([]any, len(a))
		for i, x := range a {
			b[i] = filterPreset(x)
		}
		return b
	case string:
		if secretURL(a) {
			return mask
		}
	}
	return v
}
func (s *Service) accounts(ctx context.Context) (map[string][]int64, error) {
	out := map[string][]int64{"cloud": {}, "orca_cloud": {}}
	for source, col := range map[string]string{"cloud": "cloud_token", "orca_cloud": "orca_cloud_token"} {
		key := col
		if source == "cloud" {
			key = "bambu_cloud_token"
		}
		var has bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key=? AND trim(value)!='')", key).Scan(&has); err != nil {
			return nil, err
		}
		if has {
			out[source] = append(out[source], 0)
		}
		users, err := rows(ctx, s.db, "SELECT id FROM users WHERE is_active AND trim("+col+")!='' ORDER BY id")
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			out[source] = append(out[source], ident(u["id"]))
		}
	}
	return out, nil
}
func (s *Service) cloud(ctx context.Context, files map[string]obj) (obj, error) {
	accts, e := s.accounts(ctx)
	if e != nil {
		return nil, e
	}
	summary := obj{}
	for _, source := range []string{"cloud", "orca_cloud"} {
		provider := "bambu"
		if source == "orca_cloud" {
			provider = "orca"
		}
		all := obj{}
		for _, id := range accts[source] {
			if s.CloudProfiles == nil || s.ResolvePreset == nil {
				return nil, errors.New("cloud preset reader unavailable")
			}
			name := "global"
			p := access.Principal{Admin: true}
			if id > 0 {
				name = fmt.Sprintf("user-%d", id)
				p, e = s.auth.User(ctx, id)
				if e != nil {
					return nil, e
				}
			}
			groups, state := s.CloudProfiles(ctx, p, source)
			if state != "ok" {
				all[name] = obj{"error": state}
				continue
			}
			counts := obj{}
			for _, kind := range []string{"filament", "printer", "process"} {
				presets := []obj{}
				for _, profile := range groups[kind] {
					if !truth(profile["is_custom"]) {
						continue
					}
					pid := str(profile["setting_id"])
					if pid == "" {
						pid = str(profile["id"])
					}
					if pid == "" {
						continue
					}
					value, err := s.ResolvePreset(ctx, p, source, pid, kind)
					if err != nil {
						counts["failed"] = ident(counts["failed"]) + 1
						continue
					}
					entry := obj{"name": profile["name"], "updated_time": profile["updated_time"]}
					if source == "cloud" {
						entry["setting_id"] = pid
						entry["type"] = kind
						entry["version"] = profile["version"]
						entry["base_id"] = profile["base_id"]
						entry["filament_id"] = profile["filament_id"]
						entry["setting"] = filterPreset(value)
					} else {
						entry["id"] = pid
						entry["created_time"] = profile["created_time"]
						entry["content"] = filterPreset(value)
					}
					presets = append(presets, entry)
				}
				if len(presets) > 0 {
					key := "setting_id"
					if source == "orca_cloud" {
						key = "id"
					}
					sort.Slice(presets, func(i, j int) bool { return str(presets[i][key]) < str(presets[j][key]) })
					counts[kind] = len(presets)
					files["cloud_profiles/"+provider+"/"+name+"/"+kind+".json"] = obj{"version": "2.0", "cloud": provider, "type": kind, "profiles": presets}
				}
			}
			all[name] = counts
		}
		if len(all) > 0 {
			summary[provider] = all
		}
	}
	return summary, nil
}
func (s *Service) collect(ctx context.Context, c obj) (map[string][]byte, error) {
	docs := map[string]obj{}
	contents := obj{"settings": truth(c["backup_settings"]), "spools": truth(c["backup_spools"]), "archives": truth(c["backup_archives"]), "kprofiles": false, "cloud_profiles": false}
	meta := obj{"version": "1.0", "backup_type": "bambuddy_profiles", "contents": contents}
	if truth(c["backup_kprofiles"]) {
		err := s.collectCalibrations(ctx, docs, meta)
		if err != nil {
			return nil, err
		}
		contents["kprofiles"] = len(docs) > 0
	}
	if truth(c["backup_cloud_profiles"]) {
		previous := len(docs)
		summary, e := s.cloud(ctx, docs)
		if e != nil {
			return nil, e
		}
		contents["cloud_profiles"] = len(docs) > previous
		if len(summary) > 0 {
			meta["cloud_profiles"] = summary
		}
	}
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if truth(contents["settings"]) {
		values := obj{}
		v, e := rows(ctx, tx, "SELECT key,value FROM settings ORDER BY key")
		if e != nil {
			return nil, e
		}
		for _, x := range v {
			k := str(x["key"])
			v := str(x["value"])
			if secretKey(k) {
				if strings.TrimSpace(v) != "" {
					v = mask
				}
				values[k] = v
			} else if release.Settings[k] != nil && !secretURL(v) {
				values[k] = v
			}
		}
		docs[settingsPath] = obj{"version": "1.0", "settings": values}
	}
	for _, set := range datasets {
		if !truth(contents[set.category]) {
			continue
		}
		var sqlText string
		if set.table == "archive_aliases" {
			sqlText = "SELECT merged_archive_id,archive_id FROM archive_aliases ORDER BY merged_archive_id"
		} else {
			columns := []string{}
			join := ""
			for _, k := range release.Fields[set.table] {
				if (set.table == "print_archives" && k == "created_by_username") || (set.table == "print_log_entries" && k == "owner_username") {
					columns = append(columns, "u.username AS "+k)
					join = " LEFT JOIN users u ON u.id=a.created_by_id"
				} else {
					columns = append(columns, "a."+k)
				}
			}
			sqlText = "SELECT " + strings.Join(columns, ",") + " FROM " + set.table + " a" + join + " ORDER BY a.id"
		}
		records, e := rows(ctx, tx, sqlText)
		if e != nil {
			return nil, e
		}
		for _, x := range records {
			for _, k := range []string{"makerworld_url", "external_url"} {
				if secretURL(str(x[k])) {
					x[k] = nil
				}
			}
		}
		docs[set.path] = obj{"version": "1.0", set.key: records}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	// Older backup commits may retain files for categories subsequently disabled.
	// Record the exact paths in this snapshot so a restore never selects a stale one.
	names := []string{}
	for path := range docs {
		names = append(names, path)
	}
	sort.Strings(names)
	meta["files"] = names
	docs[metadataPath] = meta
	out := map[string][]byte{}
	for name, doc := range docs {
		b, e := json.MarshalIndent(doc, "", "  ")
		if e != nil {
			return nil, e
		}
		out[name] = append(b, '\n')
	}
	return out, nil
}
