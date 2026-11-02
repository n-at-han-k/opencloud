package svc

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gatewayv1beta1 "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	userv1beta1 "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	rpcv1beta1 "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	revactx "github.com/opencloud-eu/reva/v2/pkg/ctx"
	"github.com/opencloud-eu/reva/v2/pkg/rgrpc/todo/pool"
	"github.com/riandyrn/otelchi"

	"github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/pkg/registry"
	"github.com/opencloud-eu/opencloud/pkg/tracing"
	searchsvc "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/services/search/v0"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/config"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/constants"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/dav/requests"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/generator"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/hls"
	hlscache "github.com/opencloud-eu/opencloud/services/webdav/pkg/hls/cache"
	hlsworkflow "github.com/opencloud-eu/opencloud/services/webdav/pkg/hls/workflow"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/thumbnail"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/thumbnail/cache"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/thumbnail/workflow"
)

var (
	codesEnum = map[int]string{
		http.StatusBadRequest:       "Sabre\\DAV\\Exception\\BadRequest",
		http.StatusUnauthorized:     "Sabre\\DAV\\Exception\\NotAuthenticated",
		http.StatusNotFound:         "Sabre\\DAV\\Exception\\NotFound",
		http.StatusMethodNotAllowed: "Sabre\\DAV\\Exception\\MethodNotAllowed",
	}
)

// register the REPORT method at init so it cannot race with concurrent route setup in other services.
func init() {
	chi.RegisterMethod("REPORT")
}

// Service defines the extension handlers.
type Service interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
	Thumbnail(w http.ResponseWriter, r *http.Request)
}

// NewService returns a service implementation for Service.
func NewService(opts ...Option) (Service, error) {
	options := newOptions(opts...)
	conf := options.Config

	m := chi.NewMux()
	m.Use(
		otelchi.Middleware(
			conf.Service.Name,
			otelchi.WithChiRoutes(m),
			otelchi.WithTracerProvider(options.TraceProvider),
			otelchi.WithPropagators(tracing.GetPropagator()),
		),
	)

	sourceTypes := thumbnail.NewSourceTypes(conf.Thumbnails.AdditionalSourceTypes)

	tm, err := pool.StringToTLSMode(conf.GRPCClientTLS.Mode)
	if err != nil {
		return nil, err
	}
	gatewaySelector, err := pool.GatewaySelector(conf.RevaGateway,
		pool.WithTLSCACert(conf.GRPCClientTLS.CACert),
		pool.WithTLSMode(tm),
		pool.WithRegistry(registry.GetRegistry()),
		pool.WithTracerProvider(options.TraceProvider),
	)
	if err != nil {
		return nil, err
	}

	// with previews disabled nothing talks to the generator, so the workflow
	// and its cache are never built
	var wf *workflow.ThumbnailWorkflow
	if !conf.DisablePreviews {
		wf, err = newThumbnailWorkflow(conf, sourceTypes, options.Logger, gatewaySelector)
		if err != nil {
			return nil, fmt.Errorf("create thumbnail workflow: %w", err)
		}
	}

	// HLS has its own switch. Turning previews off must not take video playback
	// with it, and the two pipelines fail independently: a broken generator URL
	// should not stop playback, a missing ffmpeg should not stop previews.
	var hwf *hlsworkflow.Workflow
	if !conf.HLS.Disable {
		hwf, err = newHLSWorkflow(conf, options.Logger, gatewaySelector)
		if err != nil {
			return nil, fmt.Errorf("create hls workflow: %w", err)
		}
	}

	svc := Webdav{
		config:          conf,
		sourceTypes:     sourceTypes,
		log:             options.Logger,
		mux:             m,
		searchClient:    searchsvc.NewSearchProviderService("eu.opencloud.api.search", conf.GrpcClient),
		workflow:        wf,
		hlsWorkflow:     hwf,
		gatewaySelector: gatewaySelector,
	}

	m.Route(options.Config.HTTP.Root, func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(svc.DavUserContext())

			r.Get("/remote.php/dav/spaces/{id}", svc.Thumbnail)
			r.Get("/remote.php/dav/spaces/{id}/*", svc.Thumbnail)
			r.Get("/dav/spaces/{id}", svc.Thumbnail)
			r.Get("/dav/spaces/{id}/*", svc.Thumbnail)

			r.Get("/remote.php/dav/files/{id}", svc.Thumbnail)
			r.Get("/remote.php/dav/files/{id}/*", svc.Thumbnail)
			r.Get("/dav/files/{id}", svc.Thumbnail)
			r.Get("/dav/files/{id}/*", svc.Thumbnail)

			r.MethodFunc("REPORT", "/remote.php/dav/spaces*", svc.Search)
			r.MethodFunc("REPORT", "/dav/spaces*", svc.Search)
			r.MethodFunc("REPORT", "/remote.php/dav/files*", svc.Search)
			r.MethodFunc("REPORT", "/dav/files*", svc.Search)
		})

		r.Group(func(r chi.Router) {
			r.Use(svc.DavPublicContext())

			r.Head("/remote.php/dav/public-files/{token}/*", svc.PublicThumbnailHead)
			r.Head("/dav/public-files/{token}/*", svc.PublicThumbnailHead)

			r.Get("/remote.php/dav/public-files/{token}/*", svc.PublicThumbnail)
			r.Get("/dav/public-files/{token}/*", svc.PublicThumbnail)
		})

		r.Group(func(r chi.Router) {
			r.Use(svc.WebDAVContext())

			r.Get("/remote.php/webdav/*", svc.Thumbnail)
			r.Get("/webdav/*", svc.Thumbnail)

			r.MethodFunc("REPORT", "/remote.php/webdav*", svc.Search)
			r.MethodFunc("REPORT", "/webdav*", svc.Search)
		})
	})

	_ = chi.Walk(m, func(method string, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		options.Logger.Debug().Str("method", method).Str("route", route).Int("middlewares", len(middlewares)).Msg("serving endpoint")
		return nil
	})

	return svc, nil
}

// newThumbnailWorkflow wires the preview pipeline: the generator client, the
// cache and the gateway-backed lookups.
func newThumbnailWorkflow(conf *config.Config, sourceTypes thumbnail.SourceTypes, logger log.Logger, gatewaySelector pool.Selectable[gatewayv1beta1.GatewayAPIClient]) (*workflow.ThumbnailWorkflow, error) {
	genTimeout, err := time.ParseDuration(conf.Thumbnails.GeneratorTimeout)
	if err != nil {
		return nil, fmt.Errorf("parse thumbnail generator timeout: %w", err)
	}
	if genTimeout == 0 {
		genTimeout = 30 * time.Second
	}

	sizes, err := thumbnail.ParseSizes(conf.Thumbnails.Resolutions)
	if err != nil {
		return nil, fmt.Errorf("parse thumbnail sizes: %w", err)
	}
	// without a ladder nothing snaps, and the client would pick the cache key
	if sizes.Max() == 0 {
		return nil, fmt.Errorf("thumbnail resolutions must name at least one edge length")
	}

	maxInputSize, err := generator.ParseMaxInputFileSize(conf.Thumbnails.MaxInputFileSize)
	if err != nil {
		return nil, fmt.Errorf("parse max input file size: %w", err)
	}

	typeMapping := thumbnail.NewTypeMapping(conf.Thumbnails.AdditionalOutputTypes)
	formats := generator.NewFormats(conf.Thumbnails.AdditionalGeneratorFormats)

	s3cfg := cache.BuildS3CacheConfig(
		conf.Thumbnails.CacheS3Bucket,
		conf.Thumbnails.CacheS3Region,
		conf.Thumbnails.CacheS3Endpoint,
		conf.Thumbnails.CacheS3AccessKey,
		conf.Thumbnails.CacheS3SecretKey,
	)

	// the new setting wins, the deprecated one is the fallback, and an empty
	// result lets the cache use its own default directory. The old setting named
	// the root the thumbnails service kept a files/ directory in. The cache goes
	// next to it, not into it, because nothing in files/ is read again and that
	// directory can then be deleted as a whole.
	cacheDir := conf.Thumbnails.CacheDir
	if cacheDir == "" && conf.Thumbnails.FilesystemStorageRoot != "" {
		cacheDir = filepath.Join(conf.Thumbnails.FilesystemStorageRoot, "cache")
	}

	c, err := cache.NewThumbnailCache(conf.Thumbnails.CacheBackend, cacheDir, s3cfg)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: genTimeout}

	return workflow.NewWorkflow(
		workflow.WithGeneratorURL(conf.Thumbnails.GeneratorURL),
		workflow.WithCache(c),
		workflow.WithHTTPClient(httpClient),
		workflow.WithMaxInputSize(maxInputSize),
		workflow.WithSizes(sizes),
		workflow.WithSourceTypes(sourceTypes),
		workflow.WithFormats(formats),
		workflow.WithTypeMapping(typeMapping),
		workflow.WithWebdavNamespace(conf.WebdavNamespace),
		workflow.WithFontMapFile(conf.Thumbnails.FontMapFile),
		workflow.WithLogger(logger),
		workflow.WithStater(workflow.NewGatewayStater(gatewaySelector)),
		workflow.WithFileDownloader(workflow.NewGatewayFileDownloader(gatewaySelector, httpClient)),
		workflow.WithSpaceLookup(workflow.NewGatewaySpaceLookup(gatewaySelector)),
		workflow.WithUserResolver(workflow.NewGatewayUserResolver(gatewaySelector)),
	)
}

// newHLSWorkflow wires the HLS pipeline: the ffmpeg executor, its own cache, the
// loopback source server and the gateway-backed lookups.
//
// It is deliberately a separate constructor with a separate cache and separate
// limits. HLS and previews share a shape, not a workload: a film's segments are
// gigabytes against a thumbnail's kilobytes, and ffmpeg's appetite for CPU has
// nothing to do with the generator's.
func newHLSWorkflow(conf *config.Config, logger log.Logger, gatewaySelector pool.Selectable[gatewayv1beta1.GatewayAPIClient]) (*hlsworkflow.Workflow, error) {
	maxInputSize, err := generator.ParseMaxInputFileSize(conf.HLS.MaxInputFileSize)
	if err != nil {
		return nil, fmt.Errorf("parse HLS max input file size: %w", err)
	}

	timeout, err := time.ParseDuration(conf.HLS.Timeout)
	if err != nil {
		return nil, fmt.Errorf("parse HLS timeout: %w", err)
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	targetDuration, err := time.ParseDuration(conf.HLS.SegmentDuration)
	if err != nil {
		return nil, fmt.Errorf("parse HLS segment duration: %w", err)
	}

	s3cfg := hlscache.BuildS3CacheConfig(
		conf.HLS.CacheS3Bucket,
		conf.HLS.CacheS3Region,
		conf.HLS.CacheS3Endpoint,
		conf.HLS.CacheS3AccessKey,
		conf.HLS.CacheS3SecretKey,
	)

	c, err := hlscache.New(conf.HLS.CacheBackend, conf.HLS.CacheDir, s3cfg)
	if err != nil {
		return nil, err
	}

	// The source server reads ranges, so its client must not carry the segment
	// timeout as a whole-body deadline: ffmpeg holds a response open while it
	// reads, and a range of a large file legitimately takes a while.
	httpClient := &http.Client{}

	return hlsworkflow.NewWorkflow(
		hlsworkflow.WithCache(c),
		hlsworkflow.WithMaxInputSize(maxInputSize),
		hlsworkflow.WithSourceTypes(hls.NewSourceTypes(conf.HLS.AdditionalSourceTypes)),
		hlsworkflow.WithTargetDuration(targetDuration),
		hlsworkflow.WithTimeout(timeout),
		hlsworkflow.WithWebdavNamespace(conf.WebdavNamespace),
		hlsworkflow.WithLogger(logger),
		hlsworkflow.WithStater(hlsworkflow.NewGatewayStater(gatewaySelector)),
		hlsworkflow.WithSpaceLookup(hlsworkflow.NewGatewaySpaceLookup(gatewaySelector)),
		hlsworkflow.WithUserResolver(hlsworkflow.NewGatewayUserResolver(gatewaySelector)),
		hlsworkflow.WithExecutor(hlsworkflow.NewFfmpegExecutor(
			conf.HLS.FfmpegBinary,
			conf.HLS.FfprobeBinary,
			conf.HLS.Concurrency,
			conf.HLS.ThreadsPerJob,
		)),
		hlsworkflow.WithSourceServer(hlsworkflow.NewSourceServer(
			hlsworkflow.NewGatewayRangeDownloader(gatewaySelector, httpClient),
			logger,
		)),
	)
}

// Webdav implements the business logic for Service.
type Webdav struct {
	sourceTypes     thumbnail.SourceTypes
	config          *config.Config
	log             log.Logger
	mux             *chi.Mux
	searchClient    searchsvc.SearchProviderService
	workflow        *workflow.ThumbnailWorkflow
	hlsWorkflow     *hlsworkflow.Workflow
	gatewaySelector pool.Selectable[gatewayv1beta1.GatewayAPIClient]
}

// ServeHTTP implements the Service interface.
func (g Webdav) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mux.ServeHTTP(w, r)
}

func (g Webdav) DavUserContext() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			filePath := r.URL.Path

			id := chi.URLParam(r, "id")
			id, err := url.QueryUnescape(id)
			if err == nil && id != "" {
				ctx = context.WithValue(ctx, constants.ContextKeyID, id)
			}

			if id != "" {
				filePath = strings.TrimPrefix(filePath, path.Join("/remote.php/dav/spaces", id))
				filePath = strings.TrimPrefix(filePath, path.Join("/dav/spaces", id))

				filePath = strings.TrimPrefix(filePath, path.Join("/remote.php/dav/files", id))
				filePath = strings.TrimPrefix(filePath, path.Join("/dav/files", id))
				filePath = strings.TrimPrefix(filePath, "/")
			}

			ctx = context.WithValue(ctx, constants.ContextKeyPath, filePath)

			next.ServeHTTP(w, r.WithContext(ctx))

		})
	}
}

func (g Webdav) DavPublicContext() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			filePath := r.URL.Path

			if token := chi.URLParam(r, "token"); token != "" {
				filePath = strings.TrimPrefix(filePath, path.Join("/remote.php/dav/public-files", token)+"/")
				filePath = strings.TrimPrefix(filePath, path.Join("/dav/public-files", token)+"/")
			}
			ctx = context.WithValue(ctx, constants.ContextKeyPath, filePath)

			next.ServeHTTP(w, r.WithContext(ctx))

		})
	}
}
func (g Webdav) WebDAVContext() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			filePath := r.URL.Path
			filePath = strings.TrimPrefix(filePath, "/remote.php")
			filePath = strings.TrimPrefix(filePath, "/webdav/")

			ctx := context.WithValue(r.Context(), constants.ContextKeyPath, filePath)

			next.ServeHTTP(w, r.WithContext(ctx))

		})
	}
}

// requestLogger is the logger every thumbnail endpoint logs through.
func (g Webdav) requestLogger(r *http.Request) log.Logger {
	return g.log.SubloggerWithRequestID(r.Context())
}

// parseThumbnailRequest reads the request every thumbnail endpoint starts with.
// It reports false when it already answered, with a 404 when previews are off
// and a 400 when the request does not parse.
func (g Webdav) parseThumbnailRequest(w http.ResponseWriter, r *http.Request, logger log.Logger) (*requests.ThumbnailRequest, bool) {
	if g.config.DisablePreviews {
		// every thumbnail path answers 404, which is what the setting promises.
		// The routes stay registered: the REPORT patterns match the same paths,
		// so without them chi would answer a thumbnail request with 405.
		http.NotFound(w, r)

		return nil, false
	}

	tr, err := requests.ParseThumbnailRequest(r)
	if err != nil {
		logger.Debug().Err(err).Msg("could not create Request")
		renderError(w, r, errBadRequest(err.Error()))

		return nil, false
	}

	return tr, true
}

// writeThumbnail runs the workflow and answers with the thumbnail it produced.
func (g Webdav) writeThumbnail(w http.ResponseWriter, r *http.Request, tr *requests.ThumbnailRequest, auth string, logger log.Logger) {
	thumb, err := g.workflow.Execute(r.Context(), tr, auth, logger)
	if err != nil {
		g.handleWorkflowError(w, r, err, tr, logger, "thumbnail")
		return
	}

	generator.WriteThumbnailResponse(w, thumb.Data, thumb.ContentType)
}

// writeHLS runs the HLS workflow and answers with the requested artifact.
func (g Webdav) writeHLS(w http.ResponseWriter, r *http.Request, hlsReq *requests.HLSRequest, auth string, logger log.Logger) {
	artifact, err := g.hlsWorkflow.Execute(r.Context(), hlsReq, auth, logger)
	if err != nil {
		g.handleHLSError(w, r, err, hlsReq.Filename, logger, "hls")
		return
	}

	writeHLSResponse(w, artifact.Data, artifact.ContentType)
}

// writeHLSResponse writes an HLS artifact to the response.
//
// Playlists carry no caching headers of their own: the artifacts are keyed on the
// source's checksum, so a changed file is a different URL's worth of content and
// a stale playlist would describe segments that no longer exist.
func writeHLSResponse(w http.ResponseWriter, data []byte, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleHLSError maps an HLS workflow error onto the response. It mirrors
// handleWorkflowError, including the 425 for a file still being processed and
// the 429 with Retry-After when the transcoder is at capacity, because those two
// are the whole reason a player retries rather than giving up.
func (g Webdav) handleHLSError(w http.ResponseWriter, r *http.Request, err error, filename string, logger log.Logger, step string) {
	var busy *hlsworkflow.GeneratorBusyError

	logger = log.Logger{Logger: logger.With().Str("step", step).Logger()}

	switch {
	case errors.Is(err, hlsworkflow.ErrFileProcessing):
		addRetryAfterHeader(w)
		renderError(w, r, errTooEarly("file is being processed"))
	case errors.As(err, &busy):
		logger.Debug().Err(err).Msg("hls transcoder is busy")
		w.Header().Set("Retry-After", strconv.Itoa(max(busy.RetryAfterSeconds, 1)))
		renderError(w, r, errTooManyRequests("the hls transcoder is busy"))
	case errors.Is(err, hlsworkflow.ErrSourceTooLarge):
		logger.Debug().Err(err).Msg("hls source too large")
		renderError(w, r, errPermissionDenied(hlsworkflow.ErrSourceTooLarge.Error()))
	case errors.Is(err, hlsworkflow.ErrPermissionDenied):
		logger.Debug().Err(err).Msg("user lacks download permission for hls")
		renderError(w, r, errPermissionDenied(hlsworkflow.ErrPermissionDenied.Error()))
	case errors.Is(err, hlsworkflow.ErrNotAFile):
		logger.Debug().Err(err).Msg("requested for a non-file resource")
		renderError(w, r, errBadRequest("Unsupported file type"))
	case errors.Is(err, hlsworkflow.ErrUnknownArtifact):
		// The selector is not one of the four artifacts, or names a segment the
		// playlist never offered. Either way the client made it up.
		logger.Debug().Err(err).Msg("unknown hls artifact")
		renderError(w, r, errBadRequest("Unknown HLS artifact"))
	case errors.Is(err, hlsworkflow.ErrNotFound):
		logger.Debug().Err(err).Msg("hls source could not be located")
		renderError(w, r, errNotFound(notFoundMsg(filename)))
	case errors.Is(err, hlsworkflow.ErrUnsupportedFileType), errors.Is(err, hlsworkflow.ErrNoVideoStream):
		logger.Debug().Err(err).Msg("hls requested for an unplayable file")
		renderError(w, r, errNotFound(notFoundMsg(filename)))
	default:
		// ffmpeg missing, a download failure, a timeout: a server error, which
		// a player may retry, rather than a 404 it would cache as unplayable.
		logger.Error().Err(err).Msg("failed")
		renderError(w, r, errInternalError("could not generate HLS artifact"))
	}
}

// Thumbnail serves a thumbnail for every authenticated DAV path, spaces, files
// and the legacy /webdav root alike: the reference is parsed from the request.
func (g Webdav) Thumbnail(w http.ResponseWriter, r *http.Request) {
	logger := g.requestLogger(r)
	if r.URL.Query().Has("hls") {
		if g.hlsWorkflow == nil {
			http.NotFound(w, r)
			return
		}

		hlsReq, err := requests.ParseHLSRequest(r)
		if err != nil {
			logger.Debug().Err(err).Msg("could not create HLS Request")
			renderError(w, r, errBadRequest(err.Error()))
			return
		}

		g.writeHLS(w, r, hlsReq, r.Header.Get(revactx.TokenHeader), logger)
		return
	}

	tr, ok := g.parseThumbnailRequest(w, r, logger)
	if !ok {
		return
	}

	g.writeThumbnail(w, r, tr, r.Header.Get(revactx.TokenHeader), logger)
}

// PublicThumbnail serves a thumbnail for a file behind a public link.
func (g Webdav) PublicThumbnail(w http.ResponseWriter, r *http.Request) {
	logger := g.requestLogger(r)
	if r.URL.Query().Has("hls") {
		if g.hlsWorkflow == nil {
			http.NotFound(w, r)
			return
		}

		hlsReq, err := requests.ParseHLSRequest(r)
		if err != nil {
			logger.Debug().Err(err).Msg("could not create HLS Request")
			renderError(w, r, errBadRequest(err.Error()))
			return
		}

		auth, err := workflow.ResolvePublicLinkAuth(r.Context(), r, chi.URLParam(r, "token"), g.gatewaySelector)
		if err != nil {
			g.handlePublicLinkAuthError(w, r, err, hlsReq.Filename, logger)
			return
		}

		g.writeHLS(w, r, hlsReq, auth, logger)
		return
	}

	tr, ok := g.parseThumbnailRequest(w, r, logger)
	if !ok {
		return
	}

	auth, err := workflow.ResolvePublicLinkAuth(r.Context(), r, chi.URLParam(r, "token"), g.gatewaySelector)
	if err != nil {
		g.handlePublicLinkAuthError(w, r, err, tr.Filename, logger)
		return
	}

	g.writeThumbnail(w, r, tr, auth, logger)
}

func whoami(gatewayClient gatewayv1beta1.GatewayAPIClient, ctx context.Context, token string) (*userv1beta1.User, error) {
	userRes, err := gatewayClient.WhoAmI(ctx, &gatewayv1beta1.WhoAmIRequest{
		Token: token,
	})
	if err != nil {
		return nil, err
	}
	if userRes.Status.Code != rpcv1beta1.Code_CODE_OK {
		return nil, fmt.Errorf("could not get user: %s", userRes.GetStatus().GetMessage())
	}
	return userRes.GetUser(), nil
}

func (g Webdav) PublicThumbnailHead(w http.ResponseWriter, r *http.Request) {
	logger := g.requestLogger(r)
	if r.URL.Query().Has("hls") {
		if g.hlsWorkflow == nil {
			http.NotFound(w, r)
			return
		}

		hlsReq, err := requests.ParseHLSRequest(r)
		if err != nil {
			logger.Debug().Err(err).Msg("could not create HLS Request")
			renderError(w, r, errBadRequest(err.Error()))
			return
		}

		auth, err := workflow.ResolvePublicLinkAuth(r.Context(), r, chi.URLParam(r, "token"), g.gatewaySelector)
		if err != nil {
			g.handlePublicLinkAuthError(w, r, err, hlsReq.Filename, logger)
			return
		}

		if err := g.hlsWorkflow.Head(r.Context(), hlsReq, auth, logger); err != nil {
			g.handleHLSError(w, r, err, hlsReq.Filename, logger, "hls head check")
			return
		}

		w.WriteHeader(http.StatusOK)
		return
	}

	tr, ok := g.parseThumbnailRequest(w, r, logger)
	if !ok {
		return
	}

	auth, err := workflow.ResolvePublicLinkAuth(r.Context(), r, chi.URLParam(r, "token"), g.gatewaySelector)
	if err != nil {
		g.handlePublicLinkAuthError(w, r, err, tr.Filename, logger)
		return
	}

	if err := g.workflow.Head(r.Context(), tr, auth, logger); err != nil {
		g.handleWorkflowError(w, r, err, tr, logger, "thumbnail head check")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleWorkflowError maps a workflow error onto the response. step names what
// was attempted, for the log line.
func (g Webdav) handleWorkflowError(w http.ResponseWriter, r *http.Request, err error, tr *requests.ThumbnailRequest, logger log.Logger, step string) {
	var busy *workflow.GeneratorBusyError

	logger = log.Logger{Logger: logger.With().Str("step", step).Logger()}

	switch {
	case errors.Is(err, workflow.ErrFileProcessing):
		addRetryAfterHeader(w)
		renderError(w, r, errTooEarly("file is being processed"))
	case errors.As(err, &busy):
		logger.Debug().Err(err).Msg("thumbnail generator is busy")
		w.Header().Set("Retry-After", strconv.Itoa(max(busy.RetryAfterSeconds, 1)))
		renderError(w, r, errTooManyRequests("the thumbnail generator is busy"))
	case errors.Is(err, workflow.ErrImageTooLarge):
		logger.Debug().Err(err).Msg("thumbnail input image too large")
		renderError(w, r, errPermissionDenied(workflow.ErrImageTooLarge.Error()))
	case errors.Is(err, workflow.ErrPermissionDenied):
		logger.Debug().Err(err).Msg("user lacks download permission for thumbnail")
		renderError(w, r, errPermissionDenied(workflow.ErrPermissionDenied.Error()))
	case errors.Is(err, workflow.ErrNotAFile):
		logger.Debug().Err(err).Msg("requested for a non-file resource")
		renderError(w, r, errBadRequest("Unsupported file type"))
	case errors.Is(err, workflow.ErrNotFound):
		logger.Debug().Err(err).Msg("thumbnail source could not be located")
		renderError(w, r, errNotFound(notFoundMsg(tr.Filename)))
	case errors.Is(err, workflow.ErrUnsupportedFileType):
		logger.Debug().Err(err).Msg("requested for an unsupported file type")
		renderError(w, r, errNotFound(notFoundMsg(tr.Filename)))
	default:
		// Anything else (generator down, download failure, timeout, ...) is a
		// server error: clients must not cache it as "no preview" the way they
		// do 404s.
		logger.Error().Err(err).Msg("failed")
		renderError(w, r, errInternalError("could not generate thumbnail"))
	}
}

func (g Webdav) handlePublicLinkAuthError(w http.ResponseWriter, r *http.Request, err error, filename string, logger log.Logger) {
	if errors.Is(err, workflow.ErrPublicLinkPasswordRequired) {
		// A password-protected public link accessed without (or with the wrong)
		// password must not reveal that the resource exists, so it is hidden
		// behind a 404 rather than a 403.
		logger.Debug().Err(err).Msg("public link requires a password")
		renderError(w, r, errNotFound(notFoundMsg(filename)))
		return
	}
	if errors.Is(err, workflow.ErrPublicLinkExpired) {
		logger.Debug().Err(err).Msg("public link has expired")
		renderError(w, r, newErrResponse(http.StatusGone, "public link has expired"))
		return
	}
	logger.Error().Err(err).Msg("could not authenticate public link")
	renderError(w, r, errInternalError("could not authenticate public link"))
}

// http://www.webdav.org/specs/rfc4918.html#ELEMENT_error
type errResponse struct {
	HTTPStatusCode int      `json:"-" xml:"-"`
	XMLName        xml.Name `xml:"d:error"`
	Xmlnsd         string   `xml:"xmlns:d,attr"`
	Xmlnss         string   `xml:"xmlns:s,attr"`
	Exception      string   `xml:"s:exception"`
	Message        string   `xml:"s:message"`
	InnerXML       []byte   `xml:",innerxml"`
}

func newErrResponse(statusCode int, msg string) *errResponse {
	rsp := &errResponse{
		HTTPStatusCode: statusCode,
		Xmlnsd:         "DAV",
		Xmlnss:         "http://sabredav.org/ns",
		Exception:      codesEnum[statusCode],
	}
	if msg != "" {
		rsp.Message = msg
	}
	return rsp
}

func errInternalError(msg string) *errResponse {
	return newErrResponse(http.StatusInternalServerError, msg)
}

func errBadRequest(msg string) *errResponse {
	return newErrResponse(http.StatusBadRequest, msg)
}

func errPermissionDenied(msg string) *errResponse {
	return newErrResponse(http.StatusForbidden, msg)
}

func errNotFound(msg string) *errResponse {
	return newErrResponse(http.StatusNotFound, msg)
}

func errTooManyRequests(msg string) *errResponse {
	return newErrResponse(http.StatusTooManyRequests, msg)
}

func errTooEarly(msg string) *errResponse {
	return newErrResponse(http.StatusTooEarly, msg)
}

func addRetryAfterHeader(w http.ResponseWriter) {
	after := rand.IntN(14) + 1
	w.Header().Set("Retry-After", strconv.Itoa(after))
}

func renderError(w http.ResponseWriter, r *http.Request, err *errResponse) {
	render.Status(r, err.HTTPStatusCode)
	render.XML(w, r, err)
}

func notFoundMsg(name string) string {
	return "File with name " + name + " could not be located"
}
