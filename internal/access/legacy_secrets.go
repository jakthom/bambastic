package access

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	fernet "github.com/fernet/fernet-go"
)

// Rewrap authenticated secrets from tagged Bambuddy backups under the installation's AES key.
func (f *secondFactor) importSecrets(tx *sql.Tx, dir string) error {
	keyText := strings.TrimSpace(os.Getenv("MFA_ENCRYPTION_KEY"))
	if keyText == "" {
		b, e := os.ReadFile(filepath.Join(dir, ".mfa_encryption_key"))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		keyText = strings.TrimSpace(string(b))
	}
	var key *fernet.Key
	if keyText != "" {
		decoded, err := fernet.DecodeKey(keyText)
		if err != nil {
			return errors.New("invalid MFA encryption key in restored installation")
		}
		key = decoded
	}
	for _, field := range [][2]string{{"user_totp", "secret"}, {"oidc_providers", "client_secret"}} {
		column := field[1]
		rows, e := tx.Query("SELECT id," + column + " FROM " + field[0] + " WHERE " + column + " <> '' AND " + column + " NOT LIKE 'bambastic:%'")
		if e != nil {
			return e
		}
		type legacy struct {
			id     int64
			secret string
		}
		var loaded []legacy
		for rows.Next() {
			var v legacy
			if e = rows.Scan(&v.id, &v.secret); e != nil {
				rows.Close()
				return e
			}
			loaded = append(loaded, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, v := range loaded {
			plain := []byte(v.secret)
			if ciphertext, ok := strings.CutPrefix(v.secret, "fernet:"); ok {
				if key == nil {
					continue
				}
				plain = fernet.VerifyAndDecrypt([]byte(ciphertext), 0, []*fernet.Key{key})
				if plain == nil {
					continue
				}
			}
			if _, e = tx.Exec("UPDATE "+field[0]+" SET "+field[1]+"=? WHERE id=? AND "+field[1]+"=?", f.encrypt(string(plain)), v.id, v.secret); e != nil {
				return e
			}
		}
	}
	return nil
}
