/*
Copyright paul_wtf.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package worldsync

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type S3Config struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey string
}

func S3ConfigFromEnv(getenv func(string) string) (S3Config, string, error) {
	cfg := S3Config{
		Endpoint:  getenv("WORLDSYNC_ENDPOINT"),
		Region:    getenv("WORLDSYNC_REGION"),
		Bucket:    getenv("WORLDSYNC_BUCKET"),
		AccessKey: getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: getenv("AWS_SECRET_ACCESS_KEY"),
	}
	for _, e := range []struct{ name, v string }{
		{"WORLDSYNC_ENDPOINT", cfg.Endpoint}, {"WORLDSYNC_REGION", cfg.Region},
		{"WORLDSYNC_BUCKET", cfg.Bucket}, {"AWS_ACCESS_KEY_ID", cfg.AccessKey},
		{"AWS_SECRET_ACCESS_KEY", cfg.SecretKey},
	} {
		if e.v == "" {
			return S3Config{}, "", fmt.Errorf("%s is not set", e.name)
		}
	}
	return cfg, getenv("WORLDSYNC_PREFIX"), nil
}

type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	return newS3Store(cfg, nil)
}

// newS3Store takes extra root CAs for the tests; nil keeps the system's.
func newS3Store(cfg S3Config, roots *x509.CertPool) (*S3Store, error) {
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		// Stores outside AWS reject or ignore the CRC headers newer SDKs add.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		// HTTP/1.1: a request that gives up closes its connection. Over HTTP/2
		// every request shares one, and the transport keeps using it after the
		// network dropped it, until TCP gives up minutes later.
		HTTPClient: awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
			tr.ForceAttemptHTTP2 = false
			tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			// The SDK clones its transport before these options run, and the
			// clone already offers h2 in ALPN: a store that picks it gets
			// HTTP/1.1 bytes on an HTTP/2 connection.
			tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
			if roots != nil {
				tr.TLSClientConfig.RootCAs = roots
			}
		}),
	})
	return &S3Store{client: client, bucket: cfg.Bucket}, nil
}

func bare(etag *string) string {
	if etag == nil {
		return ""
	}
	return strings.Trim(*etag, `"`)
}

func dateOf(md middleware.Metadata) time.Time {
	raw, ok := awsmiddleware.GetRawResponse(md).(*smithyhttp.Response)
	if !ok || raw == nil {
		return time.Time{}
	}
	d, err := http.ParseTime(raw.Header.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return d
}

func mapErr(err error) error {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		switch re.HTTPStatusCode() {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %v", ErrNotFound, err)
		case http.StatusPreconditionFailed, http.StatusConflict:
			return fmt.Errorf("%w: %v", ErrPrecondition, err)
		}
	}
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, ObjectInfo{}, mapErr(err)
	}
	return out.Body, ObjectInfo{ETag: bare(out.ETag), Size: aws.ToInt64(out.ContentLength), Date: dateOf(out.ResultMetadata), LastModified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3Store) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return ObjectInfo{}, mapErr(err)
	}
	return ObjectInfo{ETag: bare(out.ETag), Size: aws.ToInt64(out.ContentLength), Date: dateOf(out.ResultMetadata), LastModified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: body}
	if cond.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	if cond.IfMatch != "" {
		// Bare on purpose: Hetzner answers 412 to the quoted form even when
		// it matches (tried 2026-10-06).
		in.IfMatch = aws.String(cond.IfMatch)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return ObjectInfo{}, mapErr(err)
	}
	return ObjectInfo{ETag: bare(out.ETag), Date: dateOf(out.ResultMetadata)}, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil && !errors.Is(mapErr(err), ErrNotFound) {
		return mapErr(err)
	}
	return nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}
