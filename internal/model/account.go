package model

import "time"

type Account struct {
	ID              int64
	Name            string
	Provider        string // "s3"（默认）或 "sharepoint"
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string // 加密后的密文（base64）
	UsePathStyle    bool
	TenantID        string // SharePoint: Azure AD 租户 ID
	ClientID        string // SharePoint: 应用注册客户端 ID
	ClientSecret    string // SharePoint: 客户端密钥（加密存储）
	SiteURL         string // SharePoint: 站点 URL
	ReadOnly        bool      // 只读（由分享配置加载，仅内存使用，不入库）：禁止任何写操作
	ShareExpiresAt  time.Time // 分享有效期（仅内存使用，零值表示不限期）
}

func (a *Account) IsSharePoint() bool {
	return a.Provider == "sharepoint"
}

// ShareExpired 报告只读分享是否已过有效期。
func (a *Account) ShareExpired() bool {
	return !a.ShareExpiresAt.IsZero() && !time.Now().Before(a.ShareExpiresAt)
}
