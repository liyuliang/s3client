package model

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
}

func (a *Account) IsSharePoint() bool {
	return a.Provider == "sharepoint"
}
