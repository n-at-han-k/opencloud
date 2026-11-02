package requests

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	providerv1beta1 "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/go-chi/chi/v5"
	"github.com/opencloud-eu/reva/v2/pkg/storagespace"
	"github.com/opencloud-eu/reva/v2/pkg/utils"

	"github.com/opencloud-eu/opencloud/services/webdav/pkg/constants"
)

// HLSRequest is an HLS artifact requested through the hls query parameter, e.g.
// ?hls=master.m3u8 or ?hls=v0/seg00042.m4s.
//
// Artifact is the raw query value and is not validated here. The hls workflow
// parses it into a typed artifact, which is also what builds every cache key and
// playlist line, so there is exactly one place that decides what a selector
// means and nothing downstream ever handles the raw string.
type HLSRequest struct {
	Ref        *providerv1beta1.Reference
	Filename   string
	Artifact   string
	Identifier string
}

// ParseHLSRequest extracts the source reference and the artifact selector from an
// HLS playback request. Unlike a thumbnail request it carries no dimensions or
// processor parameters: the output contract is fixed.
func ParseHLSRequest(r *http.Request) (*HLSRequest, error) {
	ctx := r.Context()
	fp := ctx.Value(constants.ContextKeyPath).(string)

	var (
		ref        *providerv1beta1.Reference
		identifier string
	)

	if v := ctx.Value(constants.ContextKeyID); v != nil {
		id := v.(string)
		id = addMissingStorageID(id)
		if strings.Contains(id, "$") {
			rid, err := storagespace.ParseID(id)
			if err != nil {
				return nil, fmt.Errorf("parse space id %q: %w", id, err)
			}
			ref = &providerv1beta1.Reference{
				ResourceId: &rid,
				Path:       utils.MakeRelativePath(fp),
			}
		} else {
			ref = &providerv1beta1.Reference{Path: fp}
			identifier = id
		}
	}

	if token := chi.URLParam(r, "token"); token != "" {
		ref = &providerv1beta1.Reference{Path: filepath.Join("/public", token, strings.TrimLeft(fp, "/"))}
	} else if ref == nil {
		ref = &providerv1beta1.Reference{Path: fp}
	}

	artifact := strings.TrimSpace(r.URL.Query().Get("hls"))
	if artifact == "" {
		return nil, fmt.Errorf("missing hls artifact")
	}

	return &HLSRequest{
		Ref:        ref,
		Filename:   filepath.Base(fp),
		Artifact:   artifact,
		Identifier: identifier,
	}, nil
}
