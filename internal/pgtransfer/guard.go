package pgtransfer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Fingerprints of the data installed in a blank generation by this release.
// Numeric IDs and lifecycle timestamps are installation-specific. Comparing
// the remaining contents prevents an import from replacing edited defaults.
var initialDefaults = map[string]struct {
	count int
	hash  string
}{
	"groups":                 {3, "078e9be4e9d016b6b7e7904f3b22260b69eeb6343f95b89941b514eb845e60c0"},
	"color_catalog":          {638, "248c41ecd2356a1493700cd65d4f06799635afee573786d3a85fcfd7c54a14b2"},
	"spool_catalog":          {91, "659bb767f7181d0a68496d2e91805833d30f74cf64a903d08eb2042c2e058df3"},
	"notification_templates": {40, "f51f0667214d02308f8554edf74ef7656e59c22293265241e5277ed10d29db60"},
	"settings":               {1, "9958075fef51930af429ff6c4f7cc4b56699ab5011570a157c5faef39d710d16"},
	"bambastic_diagnostics":  {1, "0a75567d286d1ef41246edaecb7b09a9c49889fcdd5824f4ea900c0f6fad3a55"},
}

func checkDestination(ctx context.Context, tx *sql.Tx, c catalog) error {
	for _, name := range names(c) {
		var n int64
		e := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+c.name(name)).Scan(&n)
		if e != nil {
			return e
		}
		if n == 0 {
			continue
		}
		known, ok := initialDefaults[name]
		if !ok || n != int64(known.count) {
			return fmt.Errorf("destination already contains data in %q", name)
		}
		cols := []string{}
		for _, col := range c.tables[name].cols {
			switch col.name {
			case "id", "created_at", "updated_at", "timestamp":
			default:
				if !col.generated {
					cols = append(cols, quote(col.name))
				}
			}
		}
		sort.Strings(cols)
		rs, e := tx.QueryContext(ctx, "SELECT "+strings.Join(cols, ",")+" FROM "+c.name(name))
		if e != nil {
			return e
		}
		hashes := []string{}
		for rs.Next() {
			v := make([]any, len(cols))
			if e = rs.Scan(scanArgs(v)...); e != nil {
				rs.Close()
				return e
			}
			h := sha256.New()
			for _, x := range v {
				if x == nil {
					h.Write([]byte("-"))
					continue
				}
				text := fmt.Sprint(x)
				switch z := x.(type) {
				case bool:
					if z {
						text = "1"
					} else {
						text = "0"
					}
				case []byte:
					text = string(z)
				}
				fmt.Fprintf(h, "%d:%s", len(text), text)
			}
			hashes = append(hashes, fmt.Sprintf("%x", h.Sum(nil)))
		}
		e = rs.Err()
		rs.Close()
		if e != nil {
			return e
		}
		sort.Strings(hashes)
		if fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(hashes, "\n")))) != known.hash {
			return fmt.Errorf("destination contains modified defaults in %q", name)
		}
	}
	return nil
}
