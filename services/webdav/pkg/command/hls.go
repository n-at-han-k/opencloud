package command

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/opencloud-eu/opencloud/pkg/config/configlog"
	"github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/config"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/config/parser"
	"github.com/opencloud-eu/opencloud/services/webdav/pkg/hls/cache"
)

// HLSPurge is the entrypoint for the hls-purge command.
//
// Everything the HLS cache holds is derived from a source file, so purging
// costs regeneration and nothing else. It is the way out of a bad cached
// decision: a plan is probed once and then reused for the life of the file, so
// a plan made under a transient failure would otherwise stay wrong forever, and
// the key is a hash of the source rather than something an operator can aim at.
func HLSPurge(cfg *config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "hls-purge",
		Short: "discard every cached HLS plan, playlist and segment",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return configlog.ReturnError(parser.ParseConfig(cfg))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := log.Configure(cfg.Service.Name, cfg.Commons, cfg.LogLevel)

			c, err := cache.New(cfg.HLS.CacheBackend, cfg.HLS.CacheDir, cache.BuildS3CacheConfig(
				cfg.HLS.CacheS3Bucket,
				cfg.HLS.CacheS3Region,
				cfg.HLS.CacheS3Endpoint,
				cfg.HLS.CacheS3AccessKey,
				cfg.HLS.CacheS3SecretKey,
			))
			if err != nil {
				return err
			}

			if err := c.Purge(); err != nil {
				logger.Error().Err(err).Msg("could not purge the HLS cache")

				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), "HLS cache purged")

			return nil
		},
	}
}
