package awss3

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s3client/internal/model"
)

func newClient(a *model.Account) *s3.Client {
	creds := credentials.NewStaticCredentialsProvider(a.AccessKeyID, a.SecretAccessKey, "")
	region := a.Region
	if region == "" {
		region = "us-east-1"
	}
	return s3.New(s3.Options{
		Region:       region,
		Credentials:  creds,
		UsePathStyle: a.UsePathStyle,
		BaseEndpoint: endpointPtr(a.Endpoint),
	})
}

func endpointPtr(e string) *string {
	if e == "" {
		return nil
	}
	return &e
}

// TestConnection 通过 ListBuckets 验证账号凭证是否有效。
func TestConnection(a *model.Account) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := newClient(a)
	_, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	return err
}

type Object struct {
	Key          string
	Name         string
	IsDir        bool
	Size         int64
	Type         string
	LastModified time.Time
}

// fileType 根据文件名返回一个简明的类型描述。
func fileType(name string) string {
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	if ext == "" {
		return "文件"
	}
	return strings.ToUpper(ext)
}

func ListObjects(a *model.Account, bucket, prefix string) ([]Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newClient(a)
	out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    &bucket,
		Prefix:    &prefix,
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return nil, err
	}
	var objects []Object
	for _, cp := range out.CommonPrefixes {
		p := aws.ToString(cp.Prefix)
		name := p[len(prefix):]
		objects = append(objects, Object{Key: p, Name: name, IsDir: true, Type: "文件夹"})
	}
	for _, obj := range out.Contents {
		k := aws.ToString(obj.Key)
		if k == prefix {
			continue
		}
		name := k[len(prefix):]
		objects = append(objects, Object{
			Key:          k,
			Name:         name,
			IsDir:        false,
			Size:         aws.ToInt64(obj.Size),
			Type:         fileType(name),
			LastModified: aws.ToTime(obj.LastModified),
		})
	}
	return objects, nil
}

// progressReader 包装 io.ReadSeeker，每次读取后回调进度（用于上传）。
type progressReader struct {
	r     io.ReadSeeker
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

func (p *progressReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := p.r.Seek(offset, whence)
	if whence == io.SeekStart && offset == 0 {
		p.done = 0
	}
	return pos, err
}

// progressWriter 包装 io.Writer，每次写入后回调进度（用于下载）。
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

// UploadFile 上传本地文件到 bucket/key，progress 回调可为 nil。
func UploadFile(a *model.Account, bucket, key, localPath string, progress func(done, total int64)) error {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	client := newClient(a)

	body := &progressReader{r: f, total: total, cb: progress}
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &bucket,
		Key:           &key,
		Body:          body,
		ContentLength: aws.Int64(total),
	})
	return err
}

// DownloadFile 下载 bucket/key 到本地 localPath，progress 回调可为 nil。
func DownloadFile(a *model.Account, bucket, key, localPath string, progress func(done, total int64)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	client := newClient(a)

	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return err
	}
	defer out.Body.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	pw := &progressWriter{w: f, total: aws.ToInt64(out.ContentLength), cb: progress}
	_, err = io.Copy(pw, out.Body)
	return err
}

// DeleteObject 删除 bucket/key 指向的对象。
func DeleteObject(a *model.Account, bucket, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newClient(a)
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	return err
}

// CreateFolder 在 bucket 的 prefix 下创建一个名为 name 的"文件夹"（S3 用以 / 结尾的空对象表示）。
func CreateFolder(a *model.Account, bucket, prefix, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newClient(a)
	key := prefix + name
	if key == "" || key[len(key)-1] != '/' {
		key += "/"
	}
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	return err
}

// PresignURL 为 bucket/key 生成一个有效期为 expiry 的预签名 GET URL。
func PresignURL(a *model.Account, bucket, key string, expiry time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newClient(a)
	ps := s3.NewPresignClient(client)
	req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

type BucketInfo struct {
	Name       string
	Accessible bool
}

// HeadBucket 检查是否有权限访问该 bucket。
func HeadBucket(a *model.Account, bucket string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := newClient(a)
	_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket})
	return err
}

// ListBucketsWithAccess 列出账号下的桶，并用 HeadBucket 并发检查每个桶的访问权限。
func ListBucketsWithAccess(a *model.Account) ([]BucketInfo, error) {
	names, err := ListBuckets(a)
	if err != nil {
		return nil, err
	}
	infos := make([]BucketInfo, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		infos[i].Name = name
		wg.Add(1)
		go func(idx int, b string) {
			defer wg.Done()
			infos[idx].Accessible = HeadBucket(a, b) == nil
		}(i, name)
	}
	wg.Wait()
	return infos, nil
}

// ListBuckets 返回账号下的桶名列表。
func ListBuckets(a *model.Account) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := newClient(a)
	out, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	return names, nil
}
