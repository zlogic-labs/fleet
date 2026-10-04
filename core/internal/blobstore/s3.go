package blobstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config points at any S3-compatible endpoint. Ceph RGW, R2, Spaces, AWS S3
// and SeaweedFS all satisfy it, which is why the type is S3 and not the name of
// any one product.
//
// The client library is minio-go despite that. It is an S3 client, not a MinIO
// binding, and the alternative -- the AWS SDK -- is orders of magnitude larger
// for the same six operations. That this is safe rather than convenient was not
// assumed: scripts/s3-smoke.sh runs the whole path against SeaweedFS, a
// different implementation, so the code is known not to depend on one server's
// quirks.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// Prefix namespaces Fleet's objects inside a shared bucket, so a bucket
	// can be dedicated to something else without moving Fleet's data.
	Prefix string
}

func (c S3Config) validate() error {
	if c.Endpoint == "" {
		return fmt.Errorf("blobstore: endpoint is required")
	}
	if c.Bucket == "" {
		return fmt.Errorf("blobstore: bucket is required")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		// Anonymous access is legitimate for a public bucket, but it is almost
		// always a misconfiguration here, and discovering it on the first
		// failed pull costs an operator an afternoon.
		return fmt.Errorf("blobstore: access key and secret key are required")
	}
	return nil
}

// S3 is an S3-backed Store.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
	region string
}

var _ Store = (*S3)(nil)

func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	s := &S3{client: client, bucket: cfg.Bucket, region: cfg.Region, prefix: strings.Trim(cfg.Prefix, "/")}
	// A bucket that does not exist is the most common first-run failure, and
	// the error for it is much clearer now than at the first Put.
	if _, err := client.ListBuckets(ctx); err != nil {
		return nil, fmt.Errorf("blobstore: cannot reach %s: %w", cfg.Endpoint, err)
	}
	return s, nil
}

func (s *S3) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

func (s *S3) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: s.region})
}

// Put uses a single PutObject rather than a multipart upload.
//
// Multipart matters for the puller of a 140 GiB model, and the puller will
// need it; this path exists for small objects and for the case where the size
// is not known ahead of time. The puller streams through its own buffer and
// passes a known size, which is the case PutObject already handles well up to
// the 5 GiB part size.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) (Object, error) {
	full := s.key(key)
	if size < 0 {
		size = 0
	}
	info, err := s.client.PutObject(ctx, s.bucket, full, r, size,
		minio.PutObjectOptions{ContentType: contentTypeFor(key)})
	if err != nil {
		return Object{}, err
	}
	return Object{Key: key, Size: info.Size, ETag: info.ETag, LastModified: info.LastModified}, nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	full := s.key(key)
	obj, err := s.client.GetObject(ctx, s.bucket, full, minio.GetObjectOptions{})
	if err != nil {
		return nil, Object{}, translate(err)
	}
	// GetObject is lazy: the request has not been made yet, so a missing key
	// surfaces here rather than at the call that made it.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, Object{}, translate(err)
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, Object{}, err
	}
	return obj, Object{Key: key, Size: info.Size, ETag: info.ETag, LastModified: info.LastModified}, nil
}

func (s *S3) List(ctx context.Context, prefix string, limit int) ([]Object, error) {
	full := s.key(prefix)
	var out []Object
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    full,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		out = append(out, Object{
			Key:          trimPrefix(obj.Key, s.prefix),
			Size:         obj.Size,
			ETag:         obj.ETag,
			LastModified: obj.LastModified,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	SortByKey(out)
	return out, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	// S3 has no way to say "delete if present", so a missing key is the
	// success case rather than an error: a retried delete must be safe.
	return s.client.RemoveObject(ctx, s.bucket, s.key(key), minio.RemoveObjectOptions{})
}

func (s *S3) Info(ctx context.Context) Info {
	info := Info{Endpoint: s.client.EndpointURL().Host, Bucket: s.bucket, Region: s.region}
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil || !exists {
		info.Reachable = false
		info.Message = fmt.Sprintf("bucket %q is not reachable from %s", s.bucket, info.Endpoint)
		return info
	}
	objs, err := s.List(ctx, s.prefix, 0)
	if err != nil {
		info.Reachable = false
		info.Message = err.Error()
		return info
	}
	info.Reachable = true
	info.UsedBytes = TotalSize(objs)
	info.ObjectCount = int64(len(objs))
	return info
}

// contentTypeFor sets a type per file suffix. Weights are read by engines, not
// browsers, so this only matters for debugging a download by hand.
func contentTypeFor(key string) string {
	switch {
	case strings.HasSuffix(key, ".json"):
		return "application/json"
	case strings.HasSuffix(key, ".safetensors"), strings.HasSuffix(key, ".bin"):
		return "application/octet-stream"
	case strings.HasSuffix(key, ".model"), strings.HasSuffix(key, ".tiktoken"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func trimPrefix(key, prefix string) string {
	if prefix == "" {
		return key
	}
	return strings.TrimPrefix(strings.TrimPrefix(key, prefix), "/")
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return err
}

// S3FromEnv builds an S3 store from the usual environment variables, so a
// Kubernetes Secret can configure it without a config file. FLEET_S3_*
// deliberately shadows the AWS_* names: a process that also talks to AWS must
// not pick up Fleet's bucket by accident.
func S3FromEnv(ctx context.Context) (*S3, error) {
	cfg := S3Config{
		Endpoint:  os.Getenv("FLEET_S3_ENDPOINT"),
		Bucket:    os.Getenv("FLEET_S3_BUCKET"),
		Region:    os.Getenv("FLEET_S3_REGION"),
		AccessKey: os.Getenv("FLEET_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("FLEET_S3_SECRET_KEY"),
		UseSSL:    strings.EqualFold(os.Getenv("FLEET_S3_USE_SSL"), "true"),
		Prefix:    os.Getenv("FLEET_S3_PREFIX"),
	}
	return NewS3(ctx, cfg)
}

// NewFromEnvOrDir picks a backend from the environment: S3 when an endpoint is
// configured, a local directory otherwise. The fallback is what makes a first
// run work with no infrastructure at all.
func NewFromEnvOrDir(ctx context.Context, fallbackDir string) (Store, error) {
	if os.Getenv("FLEET_S3_ENDPOINT") != "" {
		return S3FromEnv(ctx)
	}
	return NewFS(fallbackDir)
}
