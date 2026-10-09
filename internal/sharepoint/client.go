// Package sharepoint 用 Microsoft Graph REST API（OAuth2 客户端凭据流，app-only）
// 访问 SharePoint 文档库的文件列表与下载，不引入重型 SDK，仅用标准库 net/http。
package sharepoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	graphBase = "https://graph.microsoft.com/v1.0"
	authHost  = "https://login.microsoftonline.com"
)

// Config 是一个 SharePoint 连接所需的凭据与目标站点。
// SiteURL 形如 https://contoso.sharepoint.com/sites/TeamA （用于解析站点 ID）。
type Config struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	SiteURL      string
}

// Item 表示文档库中的一个文件或文件夹。
type Item struct {
	ID           string
	Name         string
	IsFolder     bool
	Size         int64
	LastModified time.Time
	DownloadURL  string // 仅文件有；Graph 返回的临时直链
}

// tokenCache 按 ClientID+TenantID 缓存 access token，避免每次请求都走令牌端点。
type tokenCache struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
}

var tokens sync.Map // key: tenant|client -> *tokenCache

func cacheKey(c *Config) string { return c.TenantID + "|" + c.ClientID }

// accessToken 获取（并缓存）一个 app-only access token。
func accessToken(ctx context.Context, c *Config) (string, error) {
	v, _ := tokens.LoadOrStore(cacheKey(c), &tokenCache{})
	tc := v.(*tokenCache)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.token != "" && time.Now().Before(tc.expiry) {
		return tc.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("scope", "https://graph.microsoft.com/.default")

	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", authHost, url.PathEscape(c.TenantID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取令牌失败(%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", errors.New("令牌响应为空")
	}
	tc.token = tr.AccessToken
	// 提前 60s 过期，留出时钟误差余量。
	tc.expiry = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)
	return tc.token, nil
}

// graphGet 带 token 发起一个 Graph GET 请求，解析 JSON 到 out。
func graphGet(ctx context.Context, c *Config, path string, out interface{}) error {
	token, err := accessToken(ctx, c)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Graph 请求失败(%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// siteID 把 SiteURL 解析成 Graph 的站点 ID（hostname + server-relative path）。
func siteID(ctx context.Context, c *Config) (string, error) {
	u, err := url.Parse(strings.TrimSpace(c.SiteURL))
	if err != nil || u.Host == "" {
		return "", errors.New("SiteURL 无效，应形如 https://contoso.sharepoint.com/sites/TeamA")
	}
	relPath := strings.Trim(u.Path, "/")
	var path string
	if relPath == "" {
		path = fmt.Sprintf("/sites/%s", u.Host)
	} else {
		path = fmt.Sprintf("/sites/%s:/%s", u.Host, relPath)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := graphGet(ctx, c, path, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("未能解析站点 ID")
	}
	return out.ID, nil
}

// Drive 表示站点下的一个文档库。
type Drive struct {
	ID   string
	Name string
}

// ListDrives 列出目标站点下的所有文档库（drives）。
func ListDrives(ctx context.Context, c *Config) ([]Drive, error) {
	sid, err := siteID(ctx, c)
	if err != nil {
		return nil, err
	}
	var out struct {
		Value []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := graphGet(ctx, c, fmt.Sprintf("/sites/%s/drives", url.PathEscape(sid)), &out); err != nil {
		return nil, err
	}
	drives := make([]Drive, 0, len(out.Value))
	for _, d := range out.Value {
		drives = append(drives, Drive{ID: d.ID, Name: d.Name})
	}
	return drives, nil
}

// graphItem 是 Graph driveItem 的部分字段。
type graphItem struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Size                int64     `json:"size"`
	LastModifiedDateTime time.Time `json:"lastModifiedDateTime"`
	Folder              *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	DownloadURL string `json:"@microsoft.graph.downloadUrl"`
}

func (gi graphItem) toItem() Item {
	return Item{
		ID:           gi.ID,
		Name:         gi.Name,
		IsFolder:     gi.Folder != nil,
		Size:         gi.Size,
		LastModified: gi.LastModifiedDateTime,
		DownloadURL:  gi.DownloadURL,
	}
}

// ListChildren 列出某个 drive 下指定 itemPath（相对文档库根，空串为根）的子项。
func ListChildren(ctx context.Context, c *Config, driveID, itemPath string) ([]Item, error) {
	itemPath = strings.Trim(itemPath, "/")
	var path string
	if itemPath == "" {
		path = fmt.Sprintf("/drives/%s/root/children", url.PathEscape(driveID))
	} else {
		path = fmt.Sprintf("/drives/%s/root:/%s:/children", url.PathEscape(driveID), pathEscapeSegments(itemPath))
	}
	var out struct {
		Value []graphItem `json:"value"`
	}
	if err := graphGet(ctx, c, path, &out); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(out.Value))
	for _, gi := range out.Value {
		items = append(items, gi.toItem())
	}
	return items, nil
}

// pathEscapeSegments 对路径每一段做 URL 转义但保留斜杠分隔。
func pathEscapeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

// DownloadItem 把某个 drive 下指定 itemID 的文件下载到 localPath，progress 回调可为 nil。
func DownloadItem(ctx context.Context, c *Config, driveID, itemID, localPath string, progress func(done, total int64)) error {
	var gi graphItem
	path := fmt.Sprintf("/drives/%s/items/%s", url.PathEscape(driveID), url.PathEscape(itemID))
	if err := graphGet(ctx, c, path, &gi); err != nil {
		return err
	}
	if gi.DownloadURL == "" {
		return errors.New("该项不可下载（可能是文件夹）")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gi.DownloadURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败(%d)", resp.StatusCode)
	}

	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	pw := &progressWriter{w: f, total: gi.Size, cb: progress}
	_, err = io.Copy(pw, resp.Body)
	return err
}

// TestConnection 通过列出文档库验证凭据与站点是否有效。
func TestConnection(c *Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := ListDrives(ctx, c)
	return err
}

// progressWriter 包装 io.Writer，每次写入后回调进度。
type progressWriter struct {
	w     io.Writer
	done  int64
	total int64
	cb    func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.cb != nil {
		p.cb(p.done, p.total)
	}
	return n, err
}
