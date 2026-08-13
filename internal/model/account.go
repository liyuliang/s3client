package model

type Account struct {
	ID              int64
	Name            string
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string // 加密后的密文（base64）
	UsePathStyle    bool
}
