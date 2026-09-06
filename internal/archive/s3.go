package archive

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

// S3Options are the injectable collaborators of the S3 archive.
type S3Options struct {
	// HTTPClient overrides the SDK's default client (timeouts, tracing).
	HTTPClient *http.Client
	// Clock stamps StoredAt (system clock when nil).
	Clock clock.Clock
	// OperationTimeout bounds every store call (default 30 s).
	OperationTimeout time.Duration
}

// S3 is the aws-sdk-go-v2 ObjectArchive. It speaks to AWS S3 and to any
// S3-compatible store (MinIO locally) depending on config.ArchiveConfig.
type S3 struct {
	client  *s3.Client
	clk     clock.Clock
	timeout time.Duration
}

var _ ObjectArchive = (*S3)(nil)

// NewS3 builds the archive. Static credentials are resolved from the
// SecretRefs when AccessKeyRef is set (both refs are then required);
// otherwise the SDK's default chain (task IAM role, instance profile,
// environment) is used, which is the production posture (goal PART 99).
func NewS3(ctx context.Context, cfg config.ArchiveConfig, resolver config.Resolver, opts S3Options) (*S3, error) {
	if cfg.Region == "" {
		return nil, errors.New("archive: region is required")
	}
	if cfg.AccessKeyRef.IsZero() != cfg.SecretKeyRef.IsZero() {
		return nil, errors.New("archive: access key and secret key refs must be set together")
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		// Checksums are explicit: Put always sends the SHA-256 it computed,
		// so the SDK's implicit CRC32 trailers (which some S3-compatible
		// stores reject) are not needed.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if !cfg.AccessKeyRef.IsZero() {
		if resolver == nil {
			return nil, errors.New("archive: secret resolver is required for static credentials")
		}
		access, err := resolver.Resolve(ctx, cfg.AccessKeyRef)
		if err != nil {
			return nil, fmt.Errorf("archive: resolve access key: %w", err)
		}
		secret, err := resolver.Resolve(ctx, cfg.SecretKeyRef)
		if err != nil {
			return nil, fmt.Errorf("archive: resolve secret key: %w", err)
		}
		if strings.TrimSpace(access) == "" || strings.TrimSpace(secret) == "" {
			return nil, errors.New("archive: resolved static credentials are empty")
		}
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(access, secret, "")))
	}
	if opts.HTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(opts.HTTPClient))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("archive: load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})
	clk := opts.Clock
	if clk == nil {
		clk = clock.System()
	}
	timeout := opts.OperationTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &S3{client: client, clk: clk, timeout: timeout}, nil
}

func (a *S3) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.timeout)
}

// Put implements ObjectArchive.
func (a *S3) Put(ctx context.Context, req PutRequest) (ObjectRef, error) {
	now := a.clk.Now()
	if err := req.Validate(now); err != nil {
		return ObjectRef{}, err
	}
	sum := SHA256(req.Body)
	in := &s3.PutObjectInput{
		Bucket:            aws.String(req.Bucket),
		Key:               aws.String(req.Key),
		Body:              bytes.NewReader(req.Body),
		ContentLength:     aws.Int64(int64(len(req.Body))),
		ContentType:       aws.String(contentTypeOrDefault(req.ContentType)),
		Metadata:          mergeMetadata(req.Metadata, sum),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		ChecksumSHA256:    aws.String(base64.StdEncoding.EncodeToString(sum)),
	}
	if req.Retention != nil {
		in.ObjectLockMode = types.ObjectLockMode(req.Retention.Mode)
		in.ObjectLockRetainUntilDate = aws.Time(req.Retention.Until.UTC())
	}
	cctx, cancel := a.bound(ctx)
	defer cancel()
	out, err := a.client.PutObject(cctx, in)
	if err != nil {
		return ObjectRef{}, mapError(err, "put")
	}
	ref := ObjectRef{
		Bucket: req.Bucket, Key: req.Key, VersionID: aws.ToString(out.VersionId),
		ETag: strings.Trim(aws.ToString(out.ETag), `"`), SHA256: sum, Size: int64(len(req.Body)), StoredAt: now,
	}
	ref.URI = ref.Locator().URI()
	return ref, nil
}

// Get implements ObjectArchive.
func (a *S3) Get(ctx context.Context, loc Locator) (Object, error) {
	if err := loc.Validate(); err != nil {
		return Object{}, err
	}
	cctx, cancel := a.bound(ctx)
	defer cancel()
	out, err := a.client.GetObject(cctx, &s3.GetObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key), VersionId: optString(loc.VersionID)})
	if err != nil {
		return Object{}, mapError(err, "get")
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(out.Body, MaxObjectBytes+1))
	if err != nil {
		return Object{}, errs.Wrap(err, errs.CodeProviderUnavailable, "archive: read object body")
	}
	if len(body) > MaxObjectBytes {
		return Object{}, errs.New(errs.CodeValidationFailed, "archive: object exceeds the size limit")
	}
	sum, err := verifyIntegrity(out.Metadata, body)
	if err != nil {
		return Object{}, err
	}
	ref := ObjectRef{
		Bucket: loc.Bucket, Key: loc.Key, VersionID: firstNonEmpty(aws.ToString(out.VersionId), loc.VersionID),
		ETag: strings.Trim(aws.ToString(out.ETag), `"`), SHA256: sum, Size: int64(len(body)), StoredAt: aws.ToTime(out.LastModified),
	}
	ref.URI = ref.Locator().URI()
	return Object{Ref: ref, Body: body, ContentType: aws.ToString(out.ContentType), Metadata: out.Metadata}, nil
}

// Head implements ObjectArchive.
func (a *S3) Head(ctx context.Context, loc Locator) (HeadInfo, error) {
	if err := loc.Validate(); err != nil {
		return HeadInfo{}, err
	}
	cctx, cancel := a.bound(ctx)
	defer cancel()
	out, err := a.client.HeadObject(cctx, &s3.HeadObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key), VersionId: optString(loc.VersionID)})
	if err != nil {
		return HeadInfo{}, mapError(err, "head")
	}
	ref := ObjectRef{
		Bucket: loc.Bucket, Key: loc.Key, VersionID: firstNonEmpty(aws.ToString(out.VersionId), loc.VersionID),
		ETag: strings.Trim(aws.ToString(out.ETag), `"`), Size: aws.ToInt64(out.ContentLength), StoredAt: aws.ToTime(out.LastModified),
	}
	if h := out.Metadata[MetaSHA256]; h != "" {
		if b, derr := decodeHex(h); derr == nil {
			ref.SHA256 = b
		}
	}
	ref.URI = ref.Locator().URI()
	info := HeadInfo{Ref: ref, ContentType: aws.ToString(out.ContentType), Metadata: out.Metadata, LastModified: aws.ToTime(out.LastModified)}
	if out.ObjectLockMode != "" && out.ObjectLockRetainUntilDate != nil {
		info.Retention = &Retention{Mode: RetentionMode(out.ObjectLockMode), Until: out.ObjectLockRetainUntilDate.UTC()}
	}
	return info, nil
}

// List implements ObjectArchive.
func (a *S3) List(ctx context.Context, req ListRequest) ([]ListEntry, error) {
	if err := ValidateBucket(req.Bucket); err != nil {
		return nil, err
	}
	limit := listLimit(req.Limit)
	cctx, cancel := a.bound(ctx)
	defer cancel()
	pager := s3.NewListObjectsV2Paginator(a.client, &s3.ListObjectsV2Input{Bucket: aws.String(req.Bucket), Prefix: optString(req.Prefix)})
	var out []ListEntry
	for pager.HasMorePages() && len(out) < limit {
		page, err := pager.NextPage(cctx)
		if err != nil {
			return nil, mapError(err, "list")
		}
		for _, o := range page.Contents {
			if len(out) >= limit {
				break
			}
			out = append(out, ListEntry{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), ETag: strings.Trim(aws.ToString(o.ETag), `"`), LastModified: aws.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

// Delete implements ObjectArchive. Deleting a specific version of a locked
// object is refused by the store; that refusal is reported as
// ErrRetentionLocked.
func (a *S3) Delete(ctx context.Context, loc Locator) error {
	if err := loc.Validate(); err != nil {
		return err
	}
	cctx, cancel := a.bound(ctx)
	defer cancel()
	_, err := a.client.DeleteObject(cctx, &s3.DeleteObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key), VersionId: optString(loc.VersionID)})
	if err != nil {
		if loc.VersionID != "" && isRetentionRefusal(err) {
			return errs.Wrap(ErrRetentionLocked, errs.CodeForbidden, "archive: version is under retention")
		}
		return mapError(err, "delete")
	}
	return nil
}

// isRetentionRefusal recognizes the store's refusal to delete a locked
// version: AWS answers AccessDenied; MinIO answers InvalidRequest with an
// "Object is WORM protected" message.
func isRetentionRefusal(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "AccessDenied":
		return true
	case "InvalidRequest":
		msg := strings.ToLower(ae.ErrorMessage())
		return strings.Contains(msg, "worm") || strings.Contains(msg, "retention") || strings.Contains(msg, "lock")
	}
	return false
}

// ObjectLockEnabled reports whether the bucket has Object Lock configured.
// The composition root calls it at startup when
// config.ArchiveConfig.ObjectLockRequired is set.
func (a *S3) ObjectLockEnabled(ctx context.Context, bucket string) (bool, error) {
	if err := ValidateBucket(bucket); err != nil {
		return false, err
	}
	cctx, cancel := a.bound(ctx)
	defer cancel()
	out, err := a.client.GetObjectLockConfiguration(cctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(bucket)})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "ObjectLockConfigurationNotFoundError" {
			return false, nil
		}
		return false, mapError(err, "object lock configuration")
	}
	return out.ObjectLockConfiguration != nil && out.ObjectLockConfiguration.ObjectLockEnabled == types.ObjectLockEnabledEnabled, nil
}

// mapError converts SDK errors into *errs.Error with a stable code and no
// endpoint or credential detail.
func mapError(err error, op string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return errs.Wrap(err, errs.CodeProviderUnavailable, "archive: "+op+" timed out")
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return errs.Wrap(ErrNotFound, errs.CodeNotFound, "archive: object not found")
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchVersion":
			return errs.Wrap(ErrNotFound, errs.CodeNotFound, "archive: object not found")
		case "NoSuchBucket":
			return errs.New(errs.CodeValidationFailed, "archive: bucket does not exist")
		case "InvalidRequest":
			if strings.Contains(strings.ToLower(ae.ErrorMessage()), "object lock") {
				return errs.Wrap(ErrObjectLockNotEnabled, errs.CodeValidationFailed, "archive: bucket has no object lock configuration")
			}
		case "AccessDenied":
			return errs.Wrap(err, errs.CodeForbidden, "archive: access denied by the store").WithField("op", op)
		}
		return errs.Wrap(err, errs.CodeProviderUnavailable, "archive: store rejected the request").WithField("op", op).WithField("code", ae.ErrorCode())
	}
	return errs.Wrap(err, errs.CodeProviderUnavailable, "archive: store call failed").WithField("op", op)
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func decodeHex(s string) ([]byte, error) {
	if len(s) != 64 {
		return nil, errors.New("archive: sha256 metadata is not 64 hex characters")
	}
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		hi, ok1 := unhex(s[2*i])
		lo, ok2 := unhex(s[2*i+1])
		if !ok1 || !ok2 {
			return nil, errors.New("archive: sha256 metadata is not hex")
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
