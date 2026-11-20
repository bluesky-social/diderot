package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3MirrorConfig names the bucket the worker writes to and, for an
// S3-compatible store (MinIO, R2), the endpoint that serves it.
// Credentials always come from the AWS SDK's default chain: a workload
// role where the platform provides one, or AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY elsewhere.
type S3MirrorConfig struct {
	Bucket string
	// Region is the signing region. Empty falls back to the SDK's own
	// resolution (AWS_REGION, the shared config profile); a region that
	// resolves nowhere is an error, never a guess.
	Region string
	// Endpoint replaces the AWS endpoint resolver with a fixed base URL.
	Endpoint string
	// PathStyle addresses objects as <endpoint>/<bucket>/<key> instead of
	// <bucket>.<endpoint>/<key>; most self-hosted stores need it.
	PathStyle bool
}

// NewS3Mirror constructs an S3Mirror backed by the AWS SDK.
func NewS3Mirror(ctx context.Context, cfg S3MirrorConfig) (*s3Mirror, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("files: S3MirrorConfig.Bucket is required")
	}
	var loadOpts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if awsCfg.Region == "" {
		return nil, errors.New("files: no S3 region: set DIDEROT_IMAGES_REGION (or AWS_REGION)")
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
	})
	return &s3Mirror{
		client: client,
		bucket: cfg.Bucket,
	}, nil
}

type s3Mirror struct {
	client *s3.Client
	bucket string
}

// Bucket reports the configured bucket name.
func (m *s3Mirror) Bucket() string { return m.bucket }

// Put uploads bytes to s3://<bucket>/<key>. ContentType is set on
// the object so a future signed-URL or proxy serves bytes with the
// right header. Idempotent on retry.
func (m *s3Mirror) Put(ctx context.Context, key string, contentType string, data []byte) error {
	_, err := m.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(m.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: nullableString(contentType),
	})
	if err != nil {
		return classifyAWSError(fmt.Errorf("PutObject %s/%s: %w", m.bucket, key, err))
	}
	return nil
}

// Get reads bytes from s3://<bucket>/<key>. Returns an error
// wrapping s3types.NoSuchKey when the object is missing — caller
// can errors.As it for a poison-classification.
func (m *s3Mirror) Get(ctx context.Context, key string) ([]byte, string, error) {
	out, err := m.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(m.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			// NoSuchKey is poison, not transient — the object
			// genuinely isn't there. Return without classifying.
			return nil, "", fmt.Errorf("GetObject %s/%s: %w (missing)", m.bucket, key, err)
		}
		return nil, "", classifyAWSError(fmt.Errorf("GetObject %s/%s: %w", m.bucket, key, err))
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		// Body read failures are network-level — classify as
		// transient by default.
		return nil, "", classifyAWSError(fmt.Errorf("read body %s/%s: %w", m.bucket, key, err))
	}
	ct := ""
	if out.ContentType != nil {
		ct = *out.ContentType
	}
	return data, ct, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
