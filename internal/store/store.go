package store

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"s3client/internal/crypto"
	"s3client/internal/model"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	key []byte
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".s3client")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// pointerFile 记录当前数据库文件的实际路径（不含数据库本身）。
func pointerFile() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "db_location"), nil
}

func dbPath() (string, error) {
	if pf, err := pointerFile(); err == nil {
		if b, e := os.ReadFile(pf); e == nil {
			p := strings.TrimSpace(string(b))
			if p != "" {
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					return "", err
				}
				return p, nil
			}
		}
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "data.db"), nil
}

// CurrentDBPath 返回当前数据库文件路径，供界面显示。
func CurrentDBPath() (string, error) {
	return dbPath()
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 跨盘 rename 会失败，退回复制 + 删除。
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		in.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		in.Close()
		out.Close()
		return err
	}
	in.Close()
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// ChangeLocation 把当前数据库文件移动到 newPath，并更新指针文件。
// 调用前应确保数据库已关闭（Store.Close）。
func ChangeLocation(newPath string) error {
	old, err := dbPath()
	if err != nil {
		return err
	}
	if newPath == old {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0700); err != nil {
		return err
	}
	if _, e := os.Stat(old); e == nil {
		if err := moveFile(old, newPath); err != nil {
			return err
		}
	}
	pf, err := pointerFile()
	if err != nil {
		return err
	}
	return os.WriteFile(pf, []byte(newPath), 0600)
}

func openDB() (*sql.DB, error) {
	p, err := dbPath()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS accounts (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL,
			endpoint        TEXT NOT NULL DEFAULT '',
			region          TEXT NOT NULL DEFAULT '',
			access_key_id   TEXT NOT NULL,
			secret_access_key TEXT NOT NULL,
			use_path_style  INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func IsInitialized() (bool, error) {
	db, err := openDB()
	if err != nil {
		return false, err
	}
	defer db.Close()
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM meta WHERE key='salt'`).Scan(&count)
	return count > 0, err
}

func Initialize(masterPassword string) (*Store, error) {
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	salt, err := crypto.GenerateSalt()
	if err != nil {
		db.Close()
		return nil, err
	}
	key, err := crypto.DeriveKey(masterPassword, salt)
	if err != nil {
		db.Close()
		return nil, err
	}
	verifier, err := crypto.CreateVerifier(key)
	if err != nil {
		db.Close()
		return nil, err
	}
	saltB64 := encodeBytes(salt)
	_, err = db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('salt', ?), ('verifier', ?)`, saltB64, verifier)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, key: key}, nil
}

func Open(masterPassword string) (*Store, error) {
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	var saltB64, verifier string
	err = db.QueryRow(`SELECT value FROM meta WHERE key='salt'`).Scan(&saltB64)
	if err != nil {
		db.Close()
		return nil, errors.New("database not initialized")
	}
	err = db.QueryRow(`SELECT value FROM meta WHERE key='verifier'`).Scan(&verifier)
	if err != nil {
		db.Close()
		return nil, errors.New("database not initialized")
	}
	salt, err := decodeBytes(saltB64)
	if err != nil {
		db.Close()
		return nil, err
	}
	key, err := crypto.DeriveKey(masterPassword, salt)
	if err != nil {
		db.Close()
		return nil, err
	}
	if !crypto.CheckVerifier(key, verifier) {
		db.Close()
		return nil, errors.New("incorrect master password")
	}
	return &Store{db: db, key: key}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) AddAccount(a *model.Account) error {
	enc, err := crypto.Encrypt(s.key, []byte(a.SecretAccessKey))
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`INSERT INTO accounts (name, endpoint, region, access_key_id, secret_access_key, use_path_style) VALUES (?,?,?,?,?,?)`,
		a.Name, a.Endpoint, a.Region, a.AccessKeyID, enc, boolToInt(a.UsePathStyle),
	)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	a.ID = id
	return nil
}

func (s *Store) ListAccounts() ([]model.Account, error) {
	rows, err := s.db.Query(`SELECT id, name, endpoint, region, access_key_id, secret_access_key, use_path_style FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []model.Account
	for rows.Next() {
		var a model.Account
		var pathStyle int
		if err := rows.Scan(&a.ID, &a.Name, &a.Endpoint, &a.Region, &a.AccessKeyID, &a.SecretAccessKey, &pathStyle); err != nil {
			return nil, err
		}
		a.UsePathStyle = pathStyle != 0
		plain, err := crypto.Decrypt(s.key, a.SecretAccessKey)
		if err != nil {
			return nil, err
		}
		a.SecretAccessKey = string(plain)
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Store) DeleteAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}

func (s *Store) UpdateAccount(a *model.Account) error {
	enc, err := crypto.Encrypt(s.key, []byte(a.SecretAccessKey))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE accounts SET name=?, endpoint=?, region=?, access_key_id=?, secret_access_key=?, use_path_style=? WHERE id=?`,
		a.Name, a.Endpoint, a.Region, a.AccessKeyID, enc, boolToInt(a.UsePathStyle), a.ID,
	)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encodeBytes(b []byte) string {
	return crypto.EncodeBase64(b)
}

func decodeBytes(s string) ([]byte, error) {
	return crypto.DecodeBase64(s)
}
