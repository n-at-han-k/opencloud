# Whitelabel the OpenCloud Web UI build as TradePortal.
# Applied at build time in web.nix to the Vue/TS source of the opencloud-eu/web
# repo (reference/opencloud-web). Companion of whitelabel.sed (the Go/IDP
# rules); this one only carries what the web repo needs.
#
# minOpenCloud is the app-store schema field that matches what deployed app
# manifests declare -- it must keep its name or the app store stops reading
# minimum versions. It gets parked before the blanket replace and restored.

# Protect the app-store data-contract field.
s/minOpenCloud/@@MIN@@/g

# The webfinger REL constant name; its VALUE stays webfinger.opencloud (shared
# protocol with the backend, which the Go relabel leaves untouched).
s/OPENCLOUD_REL/TRADEPORTAL_REL/g

# Product name -> TradePortal. "OpenCloud" is never a YAML/env/config key in
# this repo, so renaming it everywhere is self-consistent and compiles
# (OpenCloudServer -> TradePortalServer, etc.).
s/OpenCloud/TradePortal/g

# Restore the protected field.
s/@@MIN@@/minOpenCloud/g