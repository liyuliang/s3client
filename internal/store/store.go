package store

import (
	"database/sql"
	"encoding/json"
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

// ChangeMasterPassword 用新主密码重新派生密钥、重新加密所有账号密钥并更新校验器（事务保证原子性）。
func (s *Store) ChangeMasterPassword(newPassword string) error {
	accounts, err := s.ListAccounts()
	if err != nil {
		return err
	}
	salt, err := crypto.GenerateSalt()
	if err != nil {
		return err
	}
	newKey, err := crypto.DeriveKey(newPassword, salt)
	if err != nil {
		return err
	}
	verifier, err := crypto.CreateVerifier(newKey)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('salt', ?), ('verifier', ?)`, encodeBytes(salt), verifier); err != nil {
		tx.Rollback()
		return err
	}
	for _, a := range accounts {
		enc, err := crypto.Encrypt(newKey, []byte(a.SecretAccessKey))
		if err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`UPDATE accounts SET secret_access_key=? WHERE id=?`, enc, a.ID); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.key = newKey
	return nil
}

// VerifyMasterPassword 校验主密码是否正确（用库内的 salt 与 verifier 比对）。
func (s *Store) VerifyMasterPassword(password string) (bool, error) {
	var saltB64, verifier string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='salt'`).Scan(&saltB64); err != nil {
		return false, err
	}
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='verifier'`).Scan(&verifier); err != nil {
		return false, err
	}
	salt, err := decodeBytes(saltB64)
	if err != nil {
		return false, err
	}
	key, err := crypto.DeriveKey(password, salt)
	if err != nil {
		return false, err
	}
	return crypto.CheckVerifier(key, verifier), nil
}

// exportAccount 是导出文件里单个账号的明文表示（整个文件会被加密）。
type exportAccount struct {
	Name            string `json:"name"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	UsePathStyle    bool   `json:"use_path_style"`
}

// exportFile 是自包含的加密导出文件格式：记录 KDF 参数、salt 与 AES-256-GCM 载荷。
type exportFile struct {
	Version      int    `json:"version"`
	KDF          string `json:"kdf"`
	ArgonTime    uint32 `json:"argon_time"`
	ArgonMemory  uint32 `json:"argon_memory"`
	ArgonThreads uint8  `json:"argon_threads"`
	Salt         string `json:"salt"`    // base64
	Payload      string `json:"payload"` // AES-256-GCM(base64(nonce||cipher||tag))
}

// ExportAccounts 用主密码经 Argon2id 派生密钥、AES-256-GCM 加密所有账号，返回自包含的导出文件字节。
func (s *Store) ExportAccounts(password string) ([]byte, error) {
	ok, err := s.VerifyMasterPassword(password)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("主密码错误")
	}
	accounts, err := s.ListAccounts()
	if err != nil {
		return nil, err
	}
	dtos := make([]exportAccount, 0, len(accounts))
	for _, a := range accounts {
		dtos = append(dtos, exportAccount{
			Name:            a.Name,
			Endpoint:        a.Endpoint,
			Region:          a.Region,
			AccessKeyID:     a.AccessKeyID,
			SecretAccessKey: a.SecretAccessKey,
			UsePathStyle:    a.UsePathStyle,
		})
	}
	plain, err := json.Marshal(dtos)
	if err != nil {
		return nil, err
	}
	salt, err := crypto.GenerateSalt()
	if err != nil {
		return nil, err
	}
	key := crypto.DeriveKeyArgon2(password, salt)
	payload, err := crypto.Encrypt(key, plain)
	if err != nil {
		return nil, err
	}
	ef := exportFile{
		Version:      1,
		KDF:          "argon2id",
		ArgonTime:    crypto.Argon2Time,
		ArgonMemory:  crypto.Argon2Memory,
		ArgonThreads: crypto.Argon2Threads,
		Salt:         crypto.EncodeBase64(salt),
		Payload:      payload,
	}
	return json.MarshalIndent(ef, "", "  ")
}

// ImportAccounts 用主密码解密导出文件（AES-GCM 认证失败即密码错误或文件损坏），
// 把其中的账号用当前库的密钥重新加密入库，返回导入数量。
func (s *Store) ImportAccounts(password string, data []byte) (int, error) {
	var ef exportFile
	if err := json.Unmarshal(data, &ef); err != nil {
		return 0, errors.New("文件格式无效")
	}
	salt, err := crypto.DecodeBase64(ef.Salt)
	if err != nil {
		return 0, errors.New("文件格式无效")
	}
	key := crypto.DeriveKeyArgon2Params(password, salt, ef.ArgonTime, ef.ArgonMemory, ef.ArgonThreads)
	plain, err := crypto.Decrypt(key, ef.Payload)
	if err != nil {
		return 0, errors.New("主密码错误或文件已损坏")
	}
	var dtos []exportAccount
	if err := json.Unmarshal(plain, &dtos); err != nil {
		return 0, errors.New("文件内容无效")
	}
	count := 0
	for i := range dtos {
		d := dtos[i]
		a := &model.Account{
			Name:            d.Name,
			Endpoint:        d.Endpoint,
			Region:          d.Region,
			AccessKeyID:     d.AccessKeyID,
			SecretAccessKey: d.SecretAccessKey,
			UsePathStyle:    d.UsePathStyle,
		}
		if err := s.AddAccount(a); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
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
