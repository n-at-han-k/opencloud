# Whitelabel the OpenCloud build as TradePortal.
# Applied at build time to the Go source and READMEs (package.nix postPatch),
# the IDP web UI (idp-web.nix) and the web UI (web.nix). Verified against
# reference/opencloud + reference/opencloud-web with bin/whitelabel-locate.sh:
# after these rules the report shows 0 RELABEL hits.
#
# Only safe strings are replaced. Internal identifiers and shared protocol
# values are left alone -- a naive global opencloud->tradeportal replace would
# break the build or the deployment:
#
#   * Go module paths (github.com/opencloud-eu/...), OC_* env NAMES
#   * eu.opencloud.* service-discovery addresses (pinned by generators/)
#   * webfinger.opencloud (webfinger REL, shared with the web client)
#   * the .opencloud config-directory default and /var/lib, /etc defaults
#     (kept in sync here, dormant because the module sets OC_CONFIG_DIR etc.)
#
# all of which the locator classifies KEEP.

# Product name (capitalized) -> TradePortal. "OpenCloud" is never a YAML/env/
# module key (those are all lowercase), so renaming it everywhere is
# self-consistent and compiles (OpenCloudURL -> TradePortalURL, etc.).
s/OpenCloud/TradePortal/g

# Deployment-identity default paths.
s#/var/lib/opencloud#/var/lib/tradeportal#g
s#/etc/opencloud#/etc/tradeportal#g
s/".opencloud"/".tradeportal"/g

# Binary invocation shown in user-facing error/help text.
s/opencloud init/tradeportal init/g
s/opencloud-idp/tradeportal-idp/g
s/\bopenCloudURL\b/tradePortalURL/g
s/openCloudContext/tradePortalContext/g
s/opencloud_repo/tradeportal_repo/g
s/opencloud\.k8s/tradeportal.k8s/g