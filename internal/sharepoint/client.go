// Package sharepoint 用 Microsoft Graph REST API（OAuth2 客户端凭据流，app-only）
// 访问 SharePoint 文档库的文件列表与下载，不引入重型 SDK，仅用标准库 net/http。
package sharepoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	ReadOnly     bool // 只读分享：禁止上传/新建文件夹/删除
}

// ErrReadOnly 表示当前连接是只读分享，不允许写入。
var ErrReadOnly = errors.New("只读分享配置，禁止写入操作")

func checkWritable(c *Config) error {
	if c != nil && c.ReadOnly {
		return ErrReadOnly
	}
	return nil
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

// graphDo 带 token 发起任意方法的 Graph 请求，bodyLen 用于设置 Content-Length（0 表示不设置）。
// out 为 nil 时忽略响应体；2xx 之外的响应都会返回错误。
func graphDo(ctx context.Context, c *Config, method, path string, body io.Reader, bodyLen int64, contentType string, out interface{}) error {
	token, err := accessToken(ctx, c)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, graphBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bodyLen > 0 {
		req.ContentLength = bodyLen
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Graph 请求失败(%d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
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
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Size                 int64     `json:"size"`
	LastModifiedDateTime time.Time `json:"lastModifiedDateTime"`
	Folder               *struct {
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

// remotePath 构造 Graph 的 "root:/路径/文件名:" 形式的路径片段。
func remotePath(itemPath, name string) string {
	p := strings.Trim(itemPath, "/")
	if p == "" {
		return "root:/" + url.PathEscape(name) + ":"
	}
	return "root:/" + pathEscapeSegments(p) + "/" + url.PathEscape(name) + ":"
}

// simpleUploadMax 简单上传（单次 PUT）的大小上限，超过则改用上传会话分片上传。
const simpleUploadMax = 4 << 20

// chunkSize 分片大小 5MB，Graph 要求为 320KB 的整数倍。
const chunkSize = 5 << 20

// UploadItem 上传本地文件到文档库的 itemPath 目录（空串表示根目录），progress 回调可为 nil。
func UploadItem(ctx context.Context, c *Config, driveID, itemPath, localPath string, progress func(done, total int64)) error {
	if err := checkWritable(c); err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	total := info.Size()
	remote := remotePath(itemPath, filepath.Base(localPath))
	base := fmt.Sprintf("/drives/%s/%s", url.PathEscape(driveID), remote)
	if total <= simpleUploadMax {
		pr := &progressReader{r: f, total: total, cb: progress}
		return graphDo(ctx, c, http.MethodPut, base+"/content", pr, total, "application/octet-stream", nil)
	}
	return uploadBySession(ctx, c, base, f, total, progress)
}

// uploadBySession 用 createUploadSession 分片上传大文件。
func uploadBySession(ctx context.Context, c *Config, base string, f io.Reader, total int64, progress func(done, total int64)) error {
	body := []byte(`{"item":{"@microsoft.graph.conflictBehavior":"rename"}}`)
	var sess struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := graphDo(ctx, c, http.MethodPost, base+"/createUploadSession",
		bytes.NewReader(body), int64(len(body)), "application/json", &sess); err != nil {
		return err
	}
	if sess.UploadURL == "" {
		return errors.New("创建上传会话失败")
	}
	buf := make([]byte, chunkSize)
	var done int64
	for done < total {
		n, rerr := f.Read(buf)
		if n > 0 {
			start, end := done, done+int64(n)-1
			if err := putChunk(ctx, sess.UploadURL, buf[:n], start, end, total); err != nil {
				return err
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// putChunk 上传单个分片（上传会话的 URL 已带授权，无需 Authorization 头）。
func putChunk(ctx context.Context, uploadURL string, data []byte, start, end, total int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("分片上传失败(%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// CreateFolder 在文档库的 parentPath（空串为根）下新建文件夹。
func CreateFolder(ctx context.Context, c *Config, driveID, parentPath, name string) error {
	if err := checkWritable(c); err != nil {
		return err
	}
	parent := strings.Trim(parentPath, "/")
	var path string
	if parent == "" {
		path = fmt.Sprintf("/drives/%s/root/children", url.PathEscape(driveID))
	} else {
		path = fmt.Sprintf("/drives/%s/root:/%s:/children", url.PathEscape(driveID), pathEscapeSegments(parent))
	}
	payload, err := json.Marshal(map[string]interface{}{
		"name":                              name,
		"folder":                            map[string]interface{}{},
		"@microsoft.graph.conflictBehavior": "rename",
	})
	if err != nil {
		return err
	}
	return graphDo(ctx, c, http.MethodPost, path, bytes.NewReader(payload), int64(len(payload)), "application/json", nil)
}

// DeleteItem 删除文档库中指定 itemID 的文件或文件夹。
func DeleteItem(ctx context.Context, c *Config, driveID, itemID string) error {
	if err := checkWritable(c); err != nil {
		return err
	}
	path := fmt.Sprintf("/drives/%s/items/%s", url.PathEscape(driveID), url.PathEscape(itemID))
	return graphDo(ctx, c, http.MethodDelete, path, nil, 0, "", nil)
}

// progressReader 包装 io.Reader，每次读取后回调进度（用于上传）。
type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	cb    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.cb != nil {
		p.cb(p.done, p.total)
	}
	return n, err
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
