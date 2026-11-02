package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	providerv1beta1 "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"

	"github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/dav/requests"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/hls"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/hls/cache"
)

// DefaultTargetDuration is the segment length a playlist aims for. Four seconds
// is short enough that a seek lands quickly and long enough that per-segment
// overhead stays small.
const DefaultTargetDuration = 4 * time.Second

// Result is one HLS artifact, ready to write to the response.
type Result struct {
	Data        []byte
	ContentType string
	Key         string
}

// Workflow owns the HLS pipeline:
//
//	resolve → stat → plan (cached) → cache check → generate → cache → respond
//
// The shape is deliberately the thumbnail workflow's, because the hard parts of
// that pipeline — resolving a DAV path to a CS3 reference, the permission and
// processing guards, keying a cache on a checksum — are the same problem. What
// is different is the middle: a thumbnail is one artifact derived in one step,
// while a stream is a plan plus a few thousand artifacts derived from it.
//
// That plan is the reason playlists are rendered here rather than by ffmpeg's
// HLS muxer. It is probed once, cached, and is then the only thing that decides
// what a playlist says and what a segment run is asked to produce, so the two
// cannot disagree. It also means master.m3u8 comes back as soon as the source
// has been probed, rather than after a transcode.
type Workflow struct {
	cache          cache.Cache
	maxInputSize   uint64
	sourceTypes    hls.SourceTypes
	targetDuration float64
	webdavNS       string
	timeout        time.Duration
	log            log.Logger

	stater       Stater
	spaceLookup  SpaceLookup
	userResolver UserResolver
	executor     Executor
	sourceServer *SourceServer
}

// Option configures a Workflow.
type Option func(*Workflow)

// WithCache sets the artifact cache.
func WithCache(c cache.Cache) Option {
	return func(w *Workflow) { w.cache = c }
}

// WithMaxInputSize sets the maximum source file size in bytes (0 = no limit).
func WithMaxInputSize(size uint64) Option {
	return func(w *Workflow) { w.maxInputSize = size }
}

// WithSourceTypes sets the types HLS playback is attempted for.
func WithSourceTypes(t hls.SourceTypes) Option {
	return func(w *Workflow) { w.sourceTypes = t }
}

// WithTargetDuration sets the segment length a playlist aims for.
func WithTargetDuration(d time.Duration) Option {
	return func(w *Workflow) { w.targetDuration = d.Seconds() }
}

// WithTimeout bounds a single request: the probe, the keyframe index or one
// segment run. It is not a bound on transcoding a whole film, because nothing
// here ever transcodes a whole film in one request.
func WithTimeout(d time.Duration) Option {
	return func(w *Workflow) { w.timeout = d }
}

// WithWebdavNamespace sets the CS3 path layout for resolving user paths.
func WithWebdavNamespace(ns string) Option {
	return func(w *Workflow) { w.webdavNS = ns }
}

// WithLogger sets the logger.
func WithLogger(l log.Logger) Option {
	return func(w *Workflow) { w.log = l }
}

// WithStater sets the file stater.
func WithStater(s Stater) Option {
	return func(w *Workflow) { w.stater = s }
}

// WithSpaceLookup sets the resolver used to look up the storage space that owns
// a path-only reference.
func WithSpaceLookup(l SpaceLookup) Option {
	return func(w *Workflow) { w.spaceLookup = l }
}

// WithUserResolver sets the resolver used to map an auth token to its user for
// path-only requests.
func WithUserResolver(r UserResolver) Option {
	return func(w *Workflow) { w.userResolver = r }
}

// WithExecutor sets the media tool runner.
func WithExecutor(e Executor) Option {
	return func(w *Workflow) { w.executor = e }
}

// WithSourceServer sets the loopback server that gives ffmpeg a seekable URL for
// a file in storage.
func WithSourceServer(s *SourceServer) Option {
	return func(w *Workflow) { w.sourceServer = s }
}

// NewWorkflow creates a Workflow.
func NewWorkflow(opts ...Option) (*Workflow, error) {
	w := &Workflow{
		targetDuration: DefaultTargetDuration.Seconds(),
		log:            log.NewLogger(log.Name("webdav")),
	}
	for _, opt := range opts {
		opt(w)
	}

	if w.stater == nil {
		return nil, fmt.Errorf("stater is required")
	}
	if w.executor == nil {
		return nil, fmt.Errorf("executor is required")
	}
	if w.sourceServer == nil {
		return nil, fmt.Errorf("source server is required")
	}
	if w.targetDuration <= 0 {
		w.targetDuration = DefaultTargetDuration.Seconds()
	}

	return w, nil
}

// Execute answers one HLS artifact request.
func (w *Workflow) Execute(ctx context.Context, req *requests.HLSRequest, auth string, logger log.Logger) (Result, error) {
	if req == nil || req.Ref == nil {
		return Result{}, fmt.Errorf("request carries no reference")
	}

	artifact, err := ParseArtifact(req.Artifact)
	if err != nil {
		return Result{}, err
	}

	if w.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.timeout)
		defer cancel()
	}

	ref, err := w.resolveReference(ctx, req.Ref, req.Identifier, auth)
	if err != nil {
		return Result{}, err
	}

	info, err := w.statSource(ctx, ref, auth, logger)
	if err != nil {
		return Result{}, err
	}

	checksum := info.GetChecksum().GetSum()
	if checksum == "" {
		// Without a checksum there is nothing stable to key on, and serving a
		// stream under a key that changes would mix segments of two versions of
		// a file into one playback.
		return Result{}, ErrNotFound
	}
	mimeType := info.GetMimeType()

	key := cacheKey(checksum, mimeType, artifact)
	if data, ok := w.cached(key, logger); ok {
		return Result{Data: data, ContentType: artifact.ContentType(), Key: key}, nil
	}

	plan, err := w.plan(ctx, ref, auth, checksum, mimeType, logger)
	if err != nil {
		return Result{}, err
	}

	switch artifact.Kind {
	case KindMaster:
		data := renderMaster(plan, versionToken(checksum, mimeType))
		w.store(key, data, logger)

		return Result{Data: data, ContentType: artifact.ContentType(), Key: key}, nil

	case KindIndex:
		rendition, err := plan.rendition(artifact.Rendition)
		if err != nil {
			return Result{}, err
		}
		data := renderIndex(*rendition, versionToken(checksum, mimeType))
		w.store(key, data, logger)

		return Result{Data: data, ContentType: artifact.ContentType(), Key: key}, nil

	case KindInit, KindSegment:
		return w.generateSegment(ctx, ref, auth, checksum, mimeType, plan, artifact, logger)

	default:
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, req.Artifact)
	}
}

// Head is the cache-and-permission check for a HEAD request: it resolves and
// stats the source without generating anything.
func (w *Workflow) Head(ctx context.Context, req *requests.HLSRequest, auth string, logger log.Logger) error {
	if req == nil || req.Ref == nil {
		return fmt.Errorf("request carries no reference")
	}

	if _, err := ParseArtifact(req.Artifact); err != nil {
		return err
	}

	ref, err := w.resolveReference(ctx, req.Ref, req.Identifier, auth)
	if err != nil {
		return err
	}

	_, err = w.statSource(ctx, ref, auth, logger)

	return err
}

// planTimeout bounds a detached plan build. The keyframe index reads the source
// end to end, and the whole point of detaching it is to let it finish.
func (w *Workflow) planTimeout() time.Duration {
	if w.timeout <= 0 {
		return 30 * time.Minute
	}

	return w.timeout
}

// plan returns the cached plan for a source, probing it if there is none.
func (w *Workflow) plan(ctx context.Context, ref *providerv1beta1.Reference, auth, checksum, mimeType string, logger log.Logger) (*Plan, error) {
	key := planCacheKey(checksum, mimeType)

	if raw, ok := w.cached(key, logger); ok {
		var plan Plan
		if err := json.Unmarshal(raw, &plan); err == nil && plan.Version == PlanVersion {
			return &plan, nil
		}
		logger.Debug().Str("key", key).Msg("discarding unreadable cached HLS plan")
	}

	// Planning is deliberately detached from the request's context.
	//
	// Indexing the keyframes of a feature film reads the whole source and takes
	// tens of seconds, which is longer than a player waits for a manifest. On
	// the request's context, the client giving up killed ffprobe mid-index and
	// the plan was then built, and cached, from the wreckage. Detached, the
	// index finishes and is cached even when nobody is still listening, so the
	// retry a moment later is served from it.
	planCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.planTimeout())
	defer cancel()

	src, release, err := w.sourceServer.Register(ref, auth)
	if err != nil {
		return nil, err
	}
	defer release()

	probe, err := w.executor.Probe(planCtx, src)
	if err != nil {
		return nil, fmt.Errorf("probe source: %w", err)
	}

	// The keyframe index is only needed to cut a copied stream, and it costs a
	// full read of the source, so it is fetched only when the video actually
	// qualifies for the copy path.
	var keyframes []float64
	if video := probe.videoStream(); video != nil && videoIsPlayable(video) {
		keyframes, err = w.executor.Keyframes(planCtx, src)
		if err != nil {
			// A failed index is not an answer. Falling back to re-encoding here
			// would cache that failure as the plan for this file and re-encode it
			// for good, so the request fails instead and the next one tries again.
			// Only a clean run that genuinely found no keyframes sends the source
			// down the re-encode path.
			return nil, fmt.Errorf("index keyframes: %w", err)
		}
		if len(keyframes) == 0 {
			logger.Debug().Msg("source reports no keyframes, re-encoding instead of copying")
		}
	}

	plan, err := buildPlan(probe, w.targetDuration, keyframes)
	if err != nil {
		return nil, err
	}

	if raw, err := json.Marshal(plan); err == nil {
		w.store(key, raw, logger)
	}

	return plan, nil
}

// generateSegment runs ffmpeg for one segment and caches both halves of its
// output. An init request runs segment zero, because the init segment is a
// by-product of any segment run and this way it is guaranteed to describe the
// same codec configuration the media segments carry.
func (w *Workflow) generateSegment(
	ctx context.Context,
	ref *providerv1beta1.Reference,
	auth, checksum, mimeType string,
	plan *Plan,
	artifact Artifact,
	logger log.Logger,
) (Result, error) {
	rendition, err := plan.rendition(artifact.Rendition)
	if err != nil {
		return Result{}, err
	}

	index := artifact.Segment
	if artifact.Kind == KindInit {
		index = 0
	}
	if index < 0 || index >= len(rendition.Segments) {
		// The playlist never names this segment, so asking for it is a bad
		// request rather than a miss.
		return Result{}, fmt.Errorf("%w: segment %d, the plan has %d",
			ErrUnknownArtifact, index, len(rendition.Segments))
	}

	src, release, err := w.sourceServer.Register(ref, auth)
	if err != nil {
		return Result{}, err
	}
	defer release()

	generated, err := w.executor.Generate(ctx, src, *rendition, rendition.Segments[index])
	if err != nil {
		return Result{}, err
	}

	initArtifact := Artifact{Kind: KindInit, Rendition: rendition.Index}
	mediaArtifact := Artifact{Kind: KindSegment, Rendition: rendition.Index, Segment: index}

	initKey := cacheKey(checksum, mimeType, initArtifact)
	mediaKey := cacheKey(checksum, mimeType, mediaArtifact)

	w.store(initKey, generated.Init, logger)
	w.store(mediaKey, generated.Media, logger)

	if artifact.Kind == KindInit {
		return Result{Data: generated.Init, ContentType: initArtifact.ContentType(), Key: initKey}, nil
	}

	return Result{Data: generated.Media, ContentType: mediaArtifact.ContentType(), Key: mediaKey}, nil
}

// cached reads one entry, treating anything other than a hit as a miss. A cache
// that is broken must not stop playback, only make it slower.
func (w *Workflow) cached(key string, logger log.Logger) ([]byte, bool) {
	if w.cache == nil {
		return nil, false
	}

	data, err := w.cache.Get(key)
	switch {
	case err == nil:
		return data, true
	case errors.Is(err, cache.ErrCacheMiss):
		return nil, false
	default:
		logger.Error().Err(err).Str("key", key).Msg("could not read from the HLS cache")

		return nil, false
	}
}

// store writes one entry. A failed write costs the next request a regeneration
// and nothing else, so it is logged and ignored.
func (w *Workflow) store(key string, data []byte, logger log.Logger) {
	if w.cache == nil || len(data) == 0 {
		return
	}

	if err := w.cache.Put(key, data); err != nil {
		logger.Debug().Err(err).Str("key", key).Msg("could not cache HLS artifact")
	}
}
