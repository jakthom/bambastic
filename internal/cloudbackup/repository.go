// Package cloudbackup stores a selective, human-readable configuration snapshot in a private Git repository.
package cloudbackup

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"bambastic/internal/lanhttp"
)

type obj = map[string]any
type repository struct{ kind, base, name, branch, token string }

var component = regexp.MustCompile(`^[[:alnum:]_.-]+$`)
var branchName = regexp.MustCompile(`^[[:alnum:]][[:alnum:]_./-]{0,159}$`)

func newRepo(raw, kind, branch, token string, allowHTTP bool) (repository, error) {
	var r repository
	if strings.HasPrefix(raw, "git@") {
		host, p, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if !ok {
			return r, errors.New("invalid repository address")
		}
		raw = "https://" + host + "/" + p
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return r, errors.New("invalid repository URL or unapproved HTTP connection")
	}
	segments := strings.Split(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if len(segments) < 2 {
		return r, errors.New("repository URL needs an owner and name")
	}
	for _, s := range segments {
		if !component.MatchString(s) || s == "." || s == ".." {
			return r, errors.New("invalid repository path")
		}
	}
	if !branchName.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".lock") {
		return r, errors.New("invalid Git branch")
	}
	if token == "" || token == "********" || strings.ContainsAny(token, "\r\n") {
		return r, errors.New("repository access token required")
	}
	repo := strings.Join(segments[len(segments)-2:], "/")
	origin := u.Scheme + "://" + u.Host
	prefix := strings.Join(segments[:len(segments)-2], "/")
	if prefix != "" {
		origin += "/" + prefix
	}
	r = repository{kind: kind, branch: branch, name: repo, token: token}
	switch kind {
	case "github":
		if u.Host == "github.com" {
			r.base = "https://api.github.com/repos/" + repo
		} else {
			r.base = origin + "/api/v3/repos/" + repo
		}
	case "gitlab":
		r.name = strings.Join(segments, "/")
		r.base = u.Scheme + "://" + u.Host + "/api/v4/projects/" + url.PathEscape(r.name)
	case "gitea", "forgejo":
		r.base = origin + "/api/v1/repos/" + repo
	default:
		return r, errors.New("unknown Git provider")
	}
	return r, nil
}

type remoteError int

func (e remoteError) Error() string { return fmt.Sprintf("repository provider returned HTTP %d", e) }
func (r repository) call(ctx context.Context, method, path string, in, out any) error {
	var reader io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, r.base+path, reader)
	if e != nil {
		return errors.New("invalid provider request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Bambastic-Backup")
	if r.kind == "gitlab" {
		req.Header.Set("PRIVATE-TOKEN", r.token)
	} else {
		req.Header.Set("Authorization", "token "+r.token)
	}
	resp, e := lanhttp.Client.Do(req)
	if e != nil {
		return errors.New("repository provider unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteError(resp.StatusCode)
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); e != nil {
			return errors.New("invalid response from repository provider")
		}
	}
	return nil
}
func (r repository) inspect(ctx context.Context) (obj, error) {
	var info obj
	if e := r.call(ctx, "GET", "", nil, &info); e != nil {
		return nil, e
	}
	v, known := info["private"].(bool)
	if r.kind == "gitlab" {
		s, ok := info["visibility"].(string)
		known = ok
		v = s == "private"
	}
	if !known || !v {
		return nil, errors.New("repository must be confirmed private")
	}
	name := r.name
	if n := str(info["full_name"]); n != "" {
		name = n
	}
	return obj{"success": true, "is_private": true, "repo_name": name, "permissions": info["permissions"], "message": "Private repository verified"}, nil
}
func (r repository) commits(ctx context.Context, ref string, limit int) ([]obj, error) {
	p := "/commits?sha=" + url.QueryEscape(ref) + "&per_page=" + fmt.Sprint(limit) + "&limit=" + fmt.Sprint(limit)
	if r.kind == "gitlab" {
		p = "/repository/commits?ref_name=" + url.QueryEscape(ref) + "&per_page=" + fmt.Sprint(limit)
	}
	var data []obj
	if e := r.call(ctx, "GET", p, nil, &data); e != nil {
		return nil, e
	}
	v := []obj{}
	for _, x := range data {
		var d obj
		if r.kind == "gitlab" {
			d = obj{"sha": x["id"], "message": x["message"], "author": x["author_name"], "date": x["committed_date"]}
		} else {
			commit := sub(x["commit"])
			a := sub(commit["author"])
			d = obj{"sha": x["sha"], "message": commit["message"], "author": a["name"], "date": a["date"]}
		}
		if isSHA(str(d["sha"])) {
			v = append(v, d)
		}
	}
	return v, nil
}
func isSHA(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	_, e := hex.DecodeString(ref)
	return e == nil
}
func (r repository) resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" || ref == "HEAD" {
		ref = r.branch
	}
	if isSHA(ref) {
		return ref, nil
	}
	if ref != r.branch {
		return "", errors.New("choose a commit SHA or the configured branch")
	}
	ds, e := r.commits(ctx, ref, 1)
	if e != nil {
		return "", e
	}
	if len(ds) == 0 {
		return "", remoteError(404)
	}
	return str(ds[0]["sha"]), nil
}
func (r repository) file(ctx context.Context, ref, path string) ([]byte, error) {
	p := "/contents/" + path + "?ref=" + url.QueryEscape(ref)
	if r.kind == "gitlab" {
		p = "/repository/files/" + url.PathEscape(path) + "?ref=" + url.QueryEscape(ref)
	}
	var v obj
	if e := r.call(ctx, "GET", p, nil, &v); e != nil {
		return nil, e
	}
	if v["type"] != nil && v["type"] != "file" {
		return nil, errors.New("snapshot item is not a regular file")
	}
	if v["encoding"] != "base64" {
		return nil, errors.New("unsupported file encoding")
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(str(v["content"]), "\n", ""))
}
func hash(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d%c", len(b), 0)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// branchHead creates a configured branch from the provider's default tip when the
// repository already has history. An empty result means no commit exists to fork.
func (r repository) branchHead(ctx context.Context) (string, error) {
	head, err := r.resolve(ctx, r.branch)
	if err == nil {
		return head, nil
	}
	if !errors.Is(err, remoteError(404)) && !errors.Is(err, remoteError(409)) {
		return "", err
	}
	var info obj
	if err = r.call(ctx, "GET", "", nil, &info); err != nil {
		return "", err
	}
	base := str(info["default_branch"])
	if base == "" || base == r.branch {
		return "", nil
	}
	commits, err := r.commits(ctx, base, 1)
	if errors.Is(err, remoteError(404)) || errors.Is(err, remoteError(409)) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", nil
	}
	head = str(commits[0]["sha"])
	switch r.kind {
	case "github":
		err = r.call(ctx, "POST", "/git/refs", obj{"ref": "refs/heads/" + r.branch, "sha": head}, nil)
	case "gitlab":
		err = r.call(ctx, "POST", "/repository/branches", obj{"branch": r.branch, "ref": head}, nil)
	default:
		err = r.call(ctx, "POST", "/branches", obj{"new_branch_name": r.branch, "old_ref_name": base}, nil)
	}
	return head, err
}
func confirmedCommit(sha string, n int, err error) (string, int, error) {
	if err != nil {
		return "", 0, err
	}
	if !isSHA(sha) {
		return "", 0, errors.New("missing commit ID")
	}
	return sha, n, nil
}
func (r repository) push(ctx context.Context, files map[string][]byte) (string, int, error) {
	head, e := r.branchHead(ctx)
	if e != nil {
		return "", 0, e
	}
	paths := []string{}
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	existing := map[string]bool{}
	changed := []string{}
	for _, p := range paths {
		if head != "" {
			old, e := r.file(ctx, head, p)
			if e == nil {
				existing[p] = true
				if hash(old) == hash(files[p]) {
					continue
				}
			} else if !errors.Is(e, remoteError(404)) {
				return "", 0, e
			}
		}
		changed = append(changed, p)
	}
	if len(changed) == 0 {
		return head, 0, nil
	}
	count := len(changed)
	const message = "Bambastic configuration backup"
	switch r.kind {
	case "github":
		if head == "" {
			var first obj
			// A partially seeded branch must not advertise a complete snapshot.
			if len(changed) > 1 && changed[0] == metadataPath {
				changed[0], changed[1] = changed[1], changed[0]
			}
			name := changed[0]
			seed := obj{"branch": r.branch, "message": message, "content": base64.StdEncoding.EncodeToString(files[name])}
			e = r.call(ctx, "PUT", "/contents/"+name, seed, &first)
			if errors.Is(e, remoteError(404)) || errors.Is(e, remoteError(422)) {
				var info obj
				if err := r.call(ctx, "GET", "", nil, &info); err != nil {
					return "", 0, err
				}
				if base := str(info["default_branch"]); base != "" && base != r.branch {
					delete(seed, "branch")
					if e = r.call(ctx, "PUT", "/contents/"+name, seed, &first); e == nil {
						sha := str(sub(first["commit"])["sha"])
						if !isSHA(sha) {
							return "", 0, errors.New("missing initial commit ID")
						}
						e = r.call(ctx, "POST", "/git/refs", obj{"ref": "refs/heads/" + r.branch, "sha": sha}, nil)
					}
				}
			}
			if e != nil {
				return "", 0, e
			}
			head = str(sub(first["commit"])["sha"])
			if !isSHA(head) {
				return "", 0, errors.New("missing initial commit ID")
			}
			if len(changed) == 1 {
				return head, 1, nil
			}
			changed = changed[1:]
		}
		var commit obj
		if e = r.call(ctx, "GET", "/git/commits/"+head, nil, &commit); e != nil {
			return "", 0, e
		}
		baseTree := str(sub(commit["tree"])["sha"])
		if !isSHA(baseTree) {
			return "", 0, errors.New("missing base tree")
		}
		entries := []obj{}
		for _, p := range changed {
			entries = append(entries, obj{"path": p, "mode": "100644", "type": "blob", "content": string(files[p])})
		}
		var tree, newCommit obj
		if e = r.call(ctx, "POST", "/git/trees", obj{"base_tree": baseTree, "tree": entries}, &tree); e != nil {
			return "", 0, e
		}
		if e = r.call(ctx, "POST", "/git/commits", obj{"message": message, "tree": tree["sha"], "parents": []string{head}}, &newCommit); e != nil {
			return "", 0, e
		}
		sha := str(newCommit["sha"])
		if !isSHA(sha) {
			return "", 0, errors.New("missing commit ID")
		}
		e = r.call(ctx, "PATCH", "/git/refs/heads/"+r.branch, obj{"sha": sha, "force": false}, nil)
		return confirmedCommit(sha, count, e)
	case "gitlab":
		actions := []obj{}
		for _, p := range changed {
			op := "create"
			if existing[p] {
				op = "update"
			}
			a := obj{"action": op, "file_path": p, "content": string(files[p])}
			if head != "" && existing[p] { // GitLab's last_commit_id is per file, not the branch tip.
				var f obj
				if e = r.call(ctx, "GET", "/repository/files/"+url.PathEscape(p)+"?ref="+head, nil, &f); e != nil {
					return "", 0, e
				}
				if f["last_commit_id"] != nil {
					a["last_commit_id"] = f["last_commit_id"]
				}
			}
			actions = append(actions, a)
		}
		var c obj
		e = r.call(ctx, "POST", "/repository/commits", obj{"branch": r.branch, "commit_message": message, "actions": actions}, &c)
		return confirmedCommit(str(c["id"]), count, e)
	default:
		entries := []obj{}
		for _, p := range changed {
			entry := obj{"operation": "create", "path": p, "content": base64.StdEncoding.EncodeToString(files[p])}
			if existing[p] {
				old, e := r.file(ctx, head, p)
				if e != nil {
					return "", 0, e
				}
				entry["operation"] = "update"
				entry["sha"] = hash(old)
			}
			entries = append(entries, entry)
		}
		var c obj
		e = r.call(ctx, "POST", "/contents", obj{"branch": r.branch, "message": message, "files": entries}, &c)
		return confirmedCommit(str(sub(c["commit"])["sha"]), count, e)
	}
}
