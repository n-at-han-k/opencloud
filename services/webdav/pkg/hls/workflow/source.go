package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	providerv1beta1 "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"

	"github.com/opencloud-eu/opencloud/pkg/log"
)

// RangeDownloader opens a read stream on a byte range of a file in storage.
// rangeHeader is passed through verbatim, or empty for the whole file.
type RangeDownloader interface {
	DownloadRange(ctx context.Context, ref *providerv1beta1.Reference, auth, rangeHeader string) (*http.Response, error)
}

// SourceServer hands local ffmpeg processes a seekable HTTP URL for a file in
// storage, translating their range requests into ranged CS3 downloads.
//
// It exists for two reasons.
//
// The first is that per-segment generation is only cheap if ffmpeg can seek.
// Downloading the whole source per request — which is what the first sketch of
// this did — means copying a 2GB film to local disk to serve four seconds of it,
// and doing it again for the next four. The data provider answers Range with 206
// (reva's rhttp/datatx/utils/download), so ffmpeg reading over HTTP fetches the
// header and the one region it needs and nothing else.
//
// The second is that the alternative — giving ffmpeg the data provider URL and
// the user's token with -headers — puts a live auth token in a command line,
// where it is readable by any other process on the box and tends to end up in
// logs and crash reports. Here the token stays in this process, and what ffmpeg
// gets is a single-use loopback URL that is forgotten as soon as the run ends.
type SourceServer struct {
	downloader RangeDownloader
	log        log.Logger

	mu       sync.Mutex
	started  bool
	baseURL  string
	listener net.Listener
	sources  map[string]*registeredSource
}

type registeredSource struct {
	ref  *providerv1beta1.Reference
	auth string
}

// NewSourceServer creates a source server. It does not listen until the first
// Register, so a deployment that never plays a video never opens a port.
func NewSourceServer(downloader RangeDownloader, logger log.Logger) *SourceServer {
	return &SourceServer{
		downloader: downloader,
		log:        logger,
		sources:    map[string]*registeredSource{},
	}
}

// Register publishes a source and returns the URL to hand ffmpeg, together with
// a release function that unpublishes it. The caller must always call release;
// a leaked registration is a URL that keeps working.
func (s *SourceServer) Register(ref *providerv1beta1.Reference, auth string) (string, func(), error) {
	if err := s.start(); err != nil {
		return "", nil, err
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, fmt.Errorf("generate source nonce: %w", err)
	}
	nonce := hex.EncodeToString(raw[:])

	s.mu.Lock()
	s.sources[nonce] = &registeredSource{ref: ref, auth: auth}
	s.mu.Unlock()

	release := func() {
		s.mu.Lock()
		delete(s.sources, nonce)
		s.mu.Unlock()
	}

	return s.baseURL + "/" + nonce, release, nil
}

// start brings up the loopback listener once.
func (s *SourceServer) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	// Loopback only: this URL carries no credential of its own beyond the nonce,
	// so it must never be reachable off the box.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for hls source server: %w", err)
	}

	s.listener = listener
	s.baseURL = "http://" + listener.Addr().String()
	s.started = true

	server := &http.Server{Handler: s}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.log.Error().Err(err).Msg("hls source server stopped")
		}
	}()

	s.log.Debug().Str("addr", s.baseURL).Msg("hls source server listening")

	return nil
}

// Close stops the listener.
func (s *SourceServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return nil
	}
	s.started = false

	return s.listener.Close()
}

func (s *SourceServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	nonce := strings.TrimPrefix(r.URL.Path, "/")
	s.mu.Lock()
	source := s.sources[nonce]
	s.mu.Unlock()

	if source == nil {
		// Either the run that owned this URL has finished, or something else on
		// the box is guessing. Both are a plain 404.
		w.WriteHeader(http.StatusNotFound)
		return
	}

	rsp, err := s.downloader.DownloadRange(r.Context(), source.ref, source.auth, r.Header.Get("Range"))
	if err != nil {
		s.log.Error().Err(err).Msg("could not read hls source range")
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer rsp.Body.Close()

	// Pass through exactly what makes the stream seekable for ffmpeg; nothing
	// else about the upstream response is any of ffmpeg's business.
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Content-Type", "Last-Modified", "ETag"} {
		if v := rsp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}

	w.WriteHeader(rsp.StatusCode)

	if r.Method == http.MethodHead {
		return
	}

	if _, err := io.Copy(w, rsp.Body); err != nil {
		// ffmpeg closing the connection after a seek is the normal case, not an
		// error worth more than a debug line.
		s.log.Debug().Err(err).Msg("hls source stream ended early")
	}
}
