package workflow

// This file is the CS3 access half of the HLS pipeline. It is a deliberate
// mirror of services/webdav/pkg/thumbnail/workflow: the stat, reference
// resolution, download and public-link code is reused as it stands there so the
// HLS experiment can move without touching the working preview path. If the
// experiment lands, this is the first thing to fold back into one shared
// package.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	gatewayv1beta1 "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	userv1beta1 "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	rpcv1beta1 "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	providerv1beta1 "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typesv1beta1 "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	revactx "github.com/opencloud-eu/reva/v2/pkg/ctx"
	"github.com/opencloud-eu/reva/v2/pkg/rgrpc/todo/pool"
	"github.com/opencloud-eu/reva/v2/pkg/storage/utils/templates"
	"github.com/opencloud-eu/reva/v2/pkg/utils"
	grpcmetadata "google.golang.org/grpc/metadata"

	"github.com/opencloud-eu/opencloud/pkg/log"
)

// Stater stats a file via the CS3 gateway.
type Stater interface {
	Stat(ctx context.Context, ref *providerv1beta1.Reference, auth string) (*providerv1beta1.StatResponse, error)
}

// FileDownloader opens a read stream on a file in storage. The returned reader
// must be consumed (and closed by the caller) exactly once; it is the raw
// response body and is never buffered or decoded here.
type FileDownloader interface {
	DownloadStream(ctx context.Context, ref *providerv1beta1.Reference, auth string) (io.ReadCloser, error)
}

// SpaceLookup resolves a path-only reference (ResourceId == nil) into a full
// reference by looking up the storage space that owns the absolute path via the
// CS3 gateway's ListStorageSpaces. This mirrors reva's ocdav spacelookup and is
// used for path-only requests (user home paths, /webdav/..., public links),
// which carry an absolute path but no space root.
type SpaceLookup interface {
	Resolve(ctx context.Context, ref *providerv1beta1.Reference, auth string) (*providerv1beta1.Reference, error)
}

// UserResolver resolves users for path-only HLS requests. WhoAmI maps an
// auth token to its user (used for /webdav/... where the URL carries no
// username); GetUserByClaim maps a username from /dav/files/{user}/... to its
// user, looked up as the requester, whose tenant the user provider searches.
type UserResolver interface {
	WhoAmI(ctx context.Context, auth string) (*userv1beta1.User, error)
	GetUserByClaim(ctx context.Context, claim, value, auth string) (*userv1beta1.User, error)
}

// ErrFileProcessing is returned when the file is still being processed by the
// storage backend (e.g. a virus scan or conversion in flight). Callers should
// surface this as HTTP 425 Too Early with a Retry-After header.
var ErrFileProcessing = fmt.Errorf("file is processing")

// ErrSourceTooLarge is returned when the source file exceeds the configured
// maximum input size. Callers should surface this as HTTP 403 Forbidden.
var ErrSourceTooLarge = errors.New("hls: image is too large")

// ErrNotAFile is returned when the requested resource is not a regular file
// (e.g. a folder). Callers should surface this as HTTP 400 Bad Request with an
// "Unsupported file type" message.
var ErrNotAFile = errors.New("hls: resource is not a file")

// ErrPermissionDenied is returned when the requesting user lacks the download
// permission on the resource (e.g. a Secure Viewer share). Callers should
// surface this as HTTP 403 Forbidden.
var ErrPermissionDenied = errors.New("hls: no download permission")

// ErrNotFound is returned when the requested file could not be located or has no
// usable checksum. Callers should surface this as HTTP 404 Not Found with a
// "File could not be located" message.
var ErrNotFound = errors.New("hls: file could not be located")

// ErrNoStorageSpace is returned when no storage space owns the requested path,
// e.g. a path in another user's home or a non-existent path. Callers should
// surface this as HTTP 404 Not Found.
var ErrNoStorageSpace = errors.New("hls: no storage space found for path")

// ErrUnsupportedFileType is returned when the file's mime type is not in the
// supported set (e.g. PDF, ODT, ZIP). Callers should surface this as HTTP 404
// Not Found with an "Unsupported file type" message.
var ErrUnsupportedFileType = errors.New("unsupported file type")

// GeneratorBusyError is returned when the local ffmpeg concurrency limit is
// reached. RetryAfterSeconds carries its own hint. The name is kept so the HTTP
// layer maps it to 429 exactly as it does for thumbnails.
type GeneratorBusyError struct {
	RetryAfterSeconds int
}

func (e *GeneratorBusyError) Error() string {
	return fmt.Sprintf("hls: transcoder is busy, retry after %ds", e.RetryAfterSeconds)
}

// fileIsProcessing reports whether the resource carries the "processing" status
// in its opaque map, as set by the storage backend while a file is being handled.
func fileIsProcessing(info *providerv1beta1.ResourceInfo) bool {
	return utils.ReadPlainFromOpaque(info.GetOpaque(), "status") == "processing"
}

// statSource stats the source and applies every guard that must hold before any
// work is done: the file exists, is a file, is not mid-processing, the caller may
// download it, its type is one we attempt, and it is within the size limit.
func (w *Workflow) statSource(ctx context.Context, ref *providerv1beta1.Reference, auth string, logger log.Logger) (*providerv1beta1.ResourceInfo, error) {
	return w.statSourceWithLimit(ctx, ref, auth, logger, w.maxInputSize)
}

// statSourceWithLimit stats the source with an explicit maximum input size.
func (w *Workflow) statSourceWithLimit(ctx context.Context, ref *providerv1beta1.Reference, auth string, logger log.Logger, maxInputSize uint64) (*providerv1beta1.ResourceInfo, error) {
	statRsp, err := w.stater.Stat(ctx, ref, auth)
	if err != nil {
		logger.Debug().Err(err).Msg("could not stat file")

		return nil, fmt.Errorf("stat: %w", err)
	}

	info := statRsp.GetInfo()

	if fileIsProcessing(info) {
		return nil, ErrFileProcessing
	}

	if !info.GetPermissionSet().GetInitiateFileDownload() {
		return nil, ErrPermissionDenied
	}

	if !w.sourceTypes.Supports(info.GetMimeType()) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFileType, info.GetMimeType())
	}

	if maxInputSize > 0 && info.GetSize() > maxInputSize {
		logger.Debug().Uint64("size", info.GetSize()).Uint64("max", maxInputSize).Msg("source file too large for HLS")

		return nil, ErrSourceTooLarge
	}

	return info, nil
}

// resolveReference turns the request's reference into a fully-anchored CS3
// reference, mirroring reva's ocdav split of space-ID-based requests from
// path-only requests:
//   - If Ref carries a ResourceId (dav/spaces/{id}/...), it is used directly,
//     the URL already identifies the space root.
//   - Otherwise (path-only request) the absolute CS3 path is resolved to the
//     user's home-relative path and, when a SpaceLookup is configured, the
//     owning storage space is looked up so the reference is anchored at the
//     space root. This also covers public links (/public/<token>/...).
func (w *Workflow) resolveReference(ctx context.Context, ref *providerv1beta1.Reference, identifier, auth string) (*providerv1beta1.Reference, error) {
	if ref == nil {
		return nil, fmt.Errorf("request carries no reference")
	}

	if ref.GetResourceId() != nil {
		// Space-ID based request: the URL carries the space root.
		return ref, nil
	}

	// Path-only request: absolutize the path relative to a user's home (the
	// legacy webdav behavior) before resolving the owning space. The user is the
	// one named in the URL (/dav/files/{user}/...) when present, otherwise the
	// token owner (/webdav/...).
	absPath := w.absolutizeUserPath(ctx, ref.GetPath(), identifier, auth)

	if w.spaceLookup == nil {
		return &providerv1beta1.Reference{Path: absPath}, nil
	}

	resolved, err := w.spaceLookup.Resolve(ctx, &providerv1beta1.Reference{Path: absPath}, auth)
	if err != nil {
		if errors.Is(err, ErrNoStorageSpace) {
			// No storage space owns the path (e.g. a file in another user's home
			// or a non-existent path). Surface it as 404.
			return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
		}
		return nil, fmt.Errorf("resolve space for path %q: %w", absPath, err)
	}
	return resolved, nil
}

// absolutizeUserPath maps a path-only reference to an absolute CS3 path under a
// user's home, matching the legacy webdav handler. The user is resolved from the
// username in the URL (identifier, via GetUserByClaim) when present, otherwise
// from the auth token (via WhoAmI). Public link paths (already absolute under
// /public) and any path that is not relative to the caller are returned
// unchanged.
func (w *Workflow) absolutizeUserPath(ctx context.Context, p, identifier, auth string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}

	if w.userResolver == nil {
		// No resolver configured: we cannot absolutize the path.
		w.log.Warn().Msg("no user resolver configured, using raw path for HLS")
		return p
	}

	var (
		user *userv1beta1.User
		err  error
	)
	if identifier != "" {
		// /dav/files/{user}/...: honor the username from the URL.
		user, err = w.userResolver.GetUserByClaim(ctx, "username", identifier, auth)
	} else {
		// /webdav/...: resolve the token owner.
		user, err = w.userResolver.WhoAmI(ctx, auth)
	}
	if err != nil {
		// Without a resolved user we cannot absolutize; fall back to the raw path.
		w.log.Warn().Err(err).Msg("could not resolve user for HLS path")
		return p
	}
	return filepath.Join(templates.WithUser(user, w.webdavNS), p)
}

type gatewaySelector interface {
	Next(...pool.Option) (gatewayv1beta1.GatewayAPIClient, error)
}

// gatewayUserResolver implements UserResolver using the CS3 gateway's WhoAmI.
type gatewayUserResolver struct {
	selector gatewaySelector
}

func (g *gatewayUserResolver) WhoAmI(ctx context.Context, auth string) (*userv1beta1.User, error) {
	client, err := g.selector.Next()
	if err != nil {
		return nil, fmt.Errorf("get gateway client: %w", err)
	}

	userRes, err := client.WhoAmI(ctx, &gatewayv1beta1.WhoAmIRequest{Token: auth})
	if err != nil {
		return nil, fmt.Errorf("whoami: %w", err)
	}
	if userRes.GetStatus().GetCode() != rpcv1beta1.Code_CODE_OK {
		return nil, fmt.Errorf("whoami: %s", userRes.GetStatus().GetMessage())
	}

	return userRes.GetUser(), nil
}

func (g *gatewayUserResolver) GetUserByClaim(ctx context.Context, claim, value, auth string) (*userv1beta1.User, error) {
	client, err := g.selector.Next()
	if err != nil {
		return nil, fmt.Errorf("get gateway client: %w", err)
	}

	ctx = grpcmetadata.AppendToOutgoingContext(ctx, revactx.TokenHeader, auth)

	userRes, err := client.GetUserByClaim(ctx, &userv1beta1.GetUserByClaimRequest{Claim: claim, Value: value})
	if err != nil {
		return nil, fmt.Errorf("get user by claim: %w", err)
	}
	if userRes.GetStatus().GetCode() != rpcv1beta1.Code_CODE_OK {
		return nil, fmt.Errorf("get user by claim: %s", userRes.GetStatus().GetMessage())
	}

	return userRes.GetUser(), nil
}

// NewGatewayUserResolver creates a UserResolver backed by the CS3 gateway.
func NewGatewayUserResolver(selector gatewaySelector) UserResolver {
	return &gatewayUserResolver{selector: selector}
}

// gatewayStater implements Stater using the CS3 gateway.
type gatewayStater struct {
	selector gatewaySelector
}

func (g *gatewayStater) Stat(ctx context.Context, ref *providerv1beta1.Reference, auth string) (*providerv1beta1.StatResponse, error) {
	client, err := g.selector.Next()
	if err != nil {
		return nil, fmt.Errorf("get gateway client: %w", err)
	}

	ctx = grpcmetadata.AppendToOutgoingContext(ctx, revactx.TokenHeader, auth)

	rsp, err := client.Stat(ctx, &providerv1beta1.StatRequest{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("stat request: %w", err)
	}

	switch rsp.GetStatus().GetCode() {
	case rpcv1beta1.Code_CODE_OK:
		// fallthrough
	case rpcv1beta1.Code_CODE_NOT_FOUND:
		// The resource does not exist or is not visible to the caller (e.g. a
		// file of another user). Surface it as 404.
		return nil, fmt.Errorf("%w: %s", ErrNotFound, rsp.GetStatus().GetMessage())
	default:
		return nil, fmt.Errorf("stat failed: %s", rsp.GetStatus().GetMessage())
	}

	if rsp.GetInfo().GetType() != providerv1beta1.ResourceType_RESOURCE_TYPE_FILE {
		return nil, ErrNotAFile
	}

	return rsp, nil
}

// gatewayFileDownloader implements FileDownloader using the CS3 gateway.
type gatewayFileDownloader struct {
	selector   gatewaySelector
	httpClient *http.Client
}

func (g *gatewayFileDownloader) DownloadStream(ctx context.Context, ref *providerv1beta1.Reference, auth string) (io.ReadCloser, error) {
	client, err := g.selector.Next()
	if err != nil {
		return nil, fmt.Errorf("get gateway client: %w", err)
	}

	ctx = grpcmetadata.AppendToOutgoingContext(ctx, revactx.TokenHeader, auth)

	rsp, err := client.InitiateFileDownload(ctx, &providerv1beta1.InitiateFileDownloadRequest{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("initiate download: %w", err)
	}

	ep, _ := extractProtocol(rsp.GetProtocols())
	if ep == "" {
		return nil, fmt.Errorf("no suitable download protocol")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	// Authenticate the data request with the user's token; the dataprovider
	// (exposed or behind the proxy) validates it directly.
	req.Header.Set(revactx.TokenHeader, auth)

	httpRsp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}

	if httpRsp.StatusCode != http.StatusOK {
		httpRsp.Body.Close()
		return nil, fmt.Errorf("download failed with status %d", httpRsp.StatusCode)
	}

	// Return the live response body; the caller owns and closes it. The bytes are
	// never buffered or decoded in webdav.
	return httpRsp.Body, nil
}

func extractProtocol(protocols []*gatewayv1beta1.FileDownloadProtocol) (endpoint, token string) {
	for _, p := range protocols {
		if p.GetProtocol() == "spaces" || p.GetProtocol() == "simple" {
			return p.GetDownloadEndpoint(), p.GetToken()
		}
	}

	for _, p := range protocols {
		if p.GetDownloadEndpoint() != "" && p.GetToken() != "" {
			return p.GetDownloadEndpoint(), p.GetToken()
		}
	}
	return "", ""
}

// NewGatewayStater creates a Stater backed by the CS3 gateway.
func NewGatewayStater(selector gatewaySelector) Stater {
	return &gatewayStater{selector: selector}
}

// NewGatewayFileDownloader creates a FileDownloader backed by the CS3 gateway.
func NewGatewayFileDownloader(selector gatewaySelector, httpClient *http.Client) FileDownloader {
	return &gatewayFileDownloader{selector: selector, httpClient: httpClient}
}

// gatewaySpaceLookup implements SpaceLookup using the CS3 gateway's
// ListStorageSpaces, mirroring reva's ocdav spacelookup.LookUpStorageSpaceForPath.
type gatewaySpaceLookup struct {
	selector gatewaySelector
}

func (g *gatewaySpaceLookup) Resolve(ctx context.Context, ref *providerv1beta1.Reference, auth string) (*providerv1beta1.Reference, error) {
	client, err := g.selector.Next()
	if err != nil {
		return nil, fmt.Errorf("get gateway client: %w", err)
	}

	ctx = grpcmetadata.AppendToOutgoingContext(ctx, revactx.TokenHeader, auth)

	lSSReq := &providerv1beta1.ListStorageSpacesRequest{
		Opaque: &typesv1beta1.Opaque{
			Map: map[string]*typesv1beta1.OpaqueEntry{
				"path":   {Decoder: "plain", Value: []byte(ref.GetPath())},
				"unique": {Decoder: "plain", Value: []byte("true")},
			},
		},
	}

	lSSRes, err := client.ListStorageSpaces(ctx, lSSReq)
	if err != nil {
		return nil, fmt.Errorf("list storage spaces: %w", err)
	}
	if lSSRes.GetStatus().GetCode() != rpcv1beta1.Code_CODE_OK {
		return nil, fmt.Errorf("list storage spaces: %s", lSSRes.GetStatus().GetMessage())
	}

	switch len(lSSRes.GetStorageSpaces()) {
	case 0:
		return nil, fmt.Errorf("%w: %q", ErrNoStorageSpace, ref.GetPath())
	case 1:
		space := lSSRes.GetStorageSpaces()[0]
		if space.GetRoot() == nil {
			return nil, fmt.Errorf("storage space for path %q has no root", ref.GetPath())
		}
		return &providerv1beta1.Reference{
			ResourceId: space.GetRoot(),
			Path:       utils.MakeRelativePath(strings.TrimPrefix(ref.GetPath(), g.spaceMountPath(space))),
		}, nil
	default:
		return nil, fmt.Errorf("too many storage spaces returned for path %q", ref.GetPath())
	}
}

// spaceMountPath extracts the mount path recorded in a storage space's opaque map,
// used to trim the absolute path down to a space-relative one. It returns "" when
// the space is not mounted at a known path (in which case the full path is kept).
func (g *gatewaySpaceLookup) spaceMountPath(space *providerv1beta1.StorageSpace) string {
	if space.GetOpaque() == nil || space.GetOpaque().GetMap()["path"] == nil {
		return ""
	}
	return string(space.GetOpaque().GetMap()["path"].GetValue())
}

// NewGatewaySpaceLookup creates a SpaceLookup backed by the CS3 gateway.
func NewGatewaySpaceLookup(selector gatewaySelector) SpaceLookup {
	return &gatewaySpaceLookup{selector: selector}
}

// ErrPublicLinkPasswordRequired is returned when a password-protected public
// link is accessed without (or with the wrong) password. Callers should surface
// this as HTTP 404 Not Found so the resource's existence is not revealed.
var ErrPublicLinkPasswordRequired = errors.New("public link requires a password")

// ErrPublicLinkExpired is returned when a public link token has expired.
// Callers should surface this as HTTP 410 Gone.
var ErrPublicLinkExpired = errors.New("public link has expired")

// ResolvePublicLinkAuth authenticates a public link token via the gateway. It
// returns ErrPublicLinkPasswordRequired or ErrPublicLinkExpired (wrapped) when
// the gateway reports the corresponding gRPC status code, so callers can branch
// on the error type instead of matching error text.
func ResolvePublicLinkAuth(ctx context.Context, r *http.Request, publicLinkToken string, selector gatewaySelector) (string, error) {
	gatewayClient, err := selector.Next()
	if err != nil {
		return "", fmt.Errorf("could not get gateway client: %w", err)
	}

	q := r.URL.Query()
	var rsp *gatewayv1beta1.AuthenticateResponse

	if q.Get("signature") != "" && q.Get("expiration") != "" {
		sig := q.Get("signature")
		exp := q.Get("expiration")
		rsp, err = gatewayClient.Authenticate(ctx, &gatewayv1beta1.AuthenticateRequest{
			Type:         "publicshares",
			ClientId:     publicLinkToken,
			ClientSecret: strings.Join([]string{"signature", sig, exp}, "|"),
		})
	} else {
		rsp, err = gatewayClient.Authenticate(ctx, &gatewayv1beta1.AuthenticateRequest{
			Type:         "publicshares",
			ClientId:     publicLinkToken,
			ClientSecret: "password|",
		})
	}

	if err != nil {
		return "", fmt.Errorf("could not authenticate public link: %w", err)
	}

	switch rsp.GetStatus().GetCode() {
	case rpcv1beta1.Code_CODE_OK:
		return rsp.GetToken(), nil
	case rpcv1beta1.Code_CODE_PERMISSION_DENIED, rpcv1beta1.Code_CODE_UNAUTHENTICATED:
		// reva reports a wrong or missing link password as InvalidCredentials,
		// which the gateway passes through as UNAUTHENTICATED.
		return "", fmt.Errorf("%w: %s", ErrPublicLinkPasswordRequired, rsp.GetStatus().GetMessage())
	case rpcv1beta1.Code_CODE_FAILED_PRECONDITION:
		return "", fmt.Errorf("%w: %s", ErrPublicLinkExpired, rsp.GetStatus().GetMessage())
	default:
		return "", fmt.Errorf("public link authentication failed: code=%s message=%s", rsp.GetStatus().GetCode(), rsp.GetStatus().GetMessage())
	}
}

// DownloadRange opens a read stream on a byte range of a file.
//
// It is the ranged sibling of DownloadStream and exists because HLS reads a
// four-second window out of a file that may be gigabytes: the data provider
// answers Range with 206, so seeking costs a range request rather than a full
// download. rangeHeader is the client's Range header, passed through verbatim,
// or empty for the whole file.
//
// The caller owns and closes the response body.
func (g *gatewayFileDownloader) DownloadRange(ctx context.Context, ref *providerv1beta1.Reference, auth, rangeHeader string) (*http.Response, error) {
	client, err := g.selector.Next()
	if err != nil {
		return nil, fmt.Errorf("get gateway client: %w", err)
	}

	ctx = grpcmetadata.AppendToOutgoingContext(ctx, revactx.TokenHeader, auth)

	rsp, err := client.InitiateFileDownload(ctx, &providerv1beta1.InitiateFileDownloadRequest{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("initiate download: %w", err)
	}

	ep, _ := extractProtocol(rsp.GetProtocols())
	if ep == "" {
		return nil, fmt.Errorf("no suitable download protocol")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set(revactx.TokenHeader, auth)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	httpRsp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}

	switch httpRsp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return httpRsp, nil
	default:
		httpRsp.Body.Close()
		return nil, fmt.Errorf("download failed with status %d", httpRsp.StatusCode)
	}
}

// NewGatewayRangeDownloader creates a RangeDownloader backed by the CS3 gateway.
func NewGatewayRangeDownloader(selector gatewaySelector, httpClient *http.Client) RangeDownloader {
	return &gatewayFileDownloader{selector: selector, httpClient: httpClient}
}
