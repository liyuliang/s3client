// Package share 实现 Bucket 的只读分享配置（.s3share）。
//
// 文件格式：明文部分只含版本、KDF 参数与 salt，桶名、有效期和访问凭据
// 全部封装在 AES-256-GCM 载荷内。因此没有分享口令既看不到 AK/SK，
// 也无法篡改有效期（GCM 认证会失败）。
package share

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"s3client/internal/crypto"
	"s3client/internal/model"
)

// Version 是当前分享配置文件格式版本。
const Version = 1

// file 是分享配置文件的明文外层结构。
type file struct {
	Version      int    `json:"version"`
	KDF          string `json:"kdf"`
	ArgonTime    uint32 `json:"argon_time"`
	ArgonMemory  uint32 `json:"argon_memory"`
	ArgonThreads uint8  `json:"argon_threads"`
	Salt         string `json:"salt"`    // base64
	Payload      string `json:"payload"` // AES-256-GCM(base64(nonce||cipher||tag))
}

// payload 是加密载荷的明文结构。
type payload struct {
	ShareName       string    `json:"share_name"`
	Bucket          string    `json:"bucket"`
	ExpiresAt       time.Time `json:"expires_at"`
	ReadOnly        bool      `json:"read_only"`
	Endpoint        string    `json:"endpoint"`
	Region          string    `json:"region"`
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	UsePathStyle    bool      `json:"use_path_style"`
}

// Session 是加载分享配置后得到的只读会话，只在内存中存在，不写入账号库。
type Session struct {
	Name      string
	Bucket    string
	ExpiresAt time.Time
	Account   *model.Account
}

// Expired 报告分享是否已过有效期。
func (s *Session) Expired() bool {
	return s.Account != nil && s.Account.ShareExpired()
}

// Remaining 返回距离过期的剩余时间描述。
func (s *Session) Remaining() string {
	if s.ExpiresAt.IsZero() {
		return "无期限"
	}
	d := time.Until(s.ExpiresAt)
	if d <= 0 {
		return "已过期"
	}
	if d < time.Hour {
		return fmt.Sprintf("剩余 %d 分钟", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("剩余 %.1f 小时", d.Hours())
	}
	return fmt.Sprintf("剩余 %.1f 天", d.Hours()/24)
}

// Create 生成一个加密的只读分享配置文件。
// ttl <= 0 表示不限制有效期；password 为分享口令，由分享者另行告知接收方。
func Create(name, bucket string, acc *model.Account, ttl time.Duration, password string) ([]byte, error) {
	if password == "" {
		return nil, errors.New("分享口令不能为空")
	}
	if bucket == "" {
		return nil, errors.New("桶名不能为空")
	}
	if acc == nil {
		return nil, errors.New("账号不能为空")
	}
	if ttl < 0 {
		return nil, errors.New("有效期不能为负数")
	}
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	plain, err := json.Marshal(payload{
		ShareName:       name,
		Bucket:          bucket,
		ExpiresAt:       expiresAt,
		ReadOnly:        true,
		Endpoint:        acc.Endpoint,
		Region:          acc.Region,
		AccessKeyID:     acc.AccessKeyID,
		SecretAccessKey: acc.SecretAccessKey,
		UsePathStyle:    acc.UsePathStyle,
	})
	if err != nil {
		return nil, err
	}
	salt, err := crypto.GenerateSalt()
	if err != nil {
		return nil, err
	}
	key := crypto.DeriveKeyArgon2(password, salt)
	enc, err := crypto.Encrypt(key, plain)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(file{
		Version:      Version,
		KDF:          "argon2id",
		ArgonTime:    crypto.Argon2Time,
		ArgonMemory:  crypto.Argon2Memory,
		ArgonThreads: crypto.Argon2Threads,
		Salt:         crypto.EncodeBase64(salt),
		Payload:      enc,
	}, "", "  ")
}

// Open 用分享口令解密配置，返回只读会话。
// 口令错误、文件损坏或被篡改都会返回错误（AES-GCM 认证失败）。
func Open(data []byte, password string) (*Session, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, errors.New("分享配置文件格式无效")
	}
	if f.Version != Version {
		return nil, fmt.Errorf("不支持的分享配置版本: %d", f.Version)
	}
	salt, err := crypto.DecodeBase64(f.Salt)
	if err != nil {
		return nil, errors.New("分享配置文件格式无效")
	}
	key := crypto.DeriveKeyArgon2Params(password, salt, f.ArgonTime, f.ArgonMemory, f.ArgonThreads)
	plain, err := crypto.Decrypt(key, f.Payload)
	if err != nil {
		return nil, errors.New("分享口令错误或文件已被修改")
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, errors.New("分享配置内容无效")
	}
	if !p.ReadOnly {
		return nil, errors.New("分享配置不是只读类型，已拒绝加载")
	}
	name := p.ShareName
	if name == "" {
		name = p.Bucket
	}
	return &Session{
		Name:      name,
		Bucket:    p.Bucket,
		ExpiresAt: p.ExpiresAt,
		Account: &model.Account{
			Name:            name,
			Provider:        "s3",
			Endpoint:        p.Endpoint,
			Region:          p.Region,
			AccessKeyID:     p.AccessKeyID,
			SecretAccessKey: p.SecretAccessKey,
			UsePathStyle:    p.UsePathStyle,
			ReadOnly:        true,
			ShareExpiresAt:  p.ExpiresAt,
		},
	}, nil
}
