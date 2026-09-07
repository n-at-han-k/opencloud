{
  lib,
  callPackage,
  buildGoModule,
  fetchFromGitHub,
  ncurses,
  gettext,
  pigeon,
  go-mockery,
  protoc-go-inject-tag,
  libxcrypt,
  vips,
  pkg-config,
  nixosTests,
  nix-update-script,
  versionCheckHook,
}:

let
  bingoBinsMakefile = builtins.concatStringsSep "\n" (
    lib.mapAttrsToList (n: v: "${n} := ${v}\n\\$(${n}):") {
      GO_XGETTEXT = "xgettext";
      MOCKERY = "mockery";
      PIGEON = "pigeon";
      PROTOC_GO_INJECT_TAG = "protoc-go-inject-tag";
    }
  );
in
buildGoModule (finalAttrs: {
  pname = "opencloud";
  version = "7.5.0";

  src = fetchFromGitHub {
    owner = "opencloud-eu";
    repo = "opencloud";
    tag = "v${finalAttrs.version}";
    hash = "sha256-OMQuNfm9xqXaDexuoDRvvSEoEb7hXEMF7ynRU3CawbI=";
  };

  postPatch = ''
    echo "${bingoBinsMakefile}" >.bingo/Variables.mk

    # Whitelabel: OpenCloud -> TradePortal across the Go source and READMEs.
    #
    # Non-Go files can carry the brand too, and when they are either shipped or
    # compared against the rewritten code they must follow: *.tmpl (mail
    # templates -- the notification tests assert a template render equals a Go
    # string that IS rewritten), *.json (theme.json is the shipped default
    # theme, fontconfiguration.json is a test fixture), *.po (l10n catalogs).
    find . -type f \( -name '*.go' -o -name '*.md' -o -name '*.tmpl' -o -name '*.po' -o -name '*.json' \) -not -path './vendor/*' -print0 | xargs -0 sed -i -f ${./whitelabel.sed}

    # tries to build web assets, done separately
    substituteInPlace services/idp/Makefile \
      --replace-fail 'node-generate-prod: assets' 'node-generate-prod:'
    # tries to download something web assets ..
    substituteInPlace services/web/Makefile \
      --replace-fail 'node-generate-prod: download-assets' 'node-generate-prod:'

    # tries to build some random binaries off the internet and
    # no need to build protobuf bindings anyway, as they are in-repo already
    sed -i -e '/\$(BINGO) get/d' -e '/\$(BUF) generate/d' .make/protobuf.mk
  '';

  vendorHash = null;

  preConfigure = ''
    export HOME=$(mktemp -d)
    make generate
  '';

  ldflags = [
    "-s"
    "-w"
    "-X"
    "github.com/opencloud-eu/opencloud/pkg/version.String=nixos"
    "-X"
    "github.com/opencloud-eu/opencloud/pkg/version.Tag=${finalAttrs.version}"
    "-X"
    "github.com/opencloud-eu/opencloud/pkg/version.Date=19700101"
    # pkg/version/version.go defaults Edition to "dev" for self-compiled
    # builds, and update.opencloud.eu/server.json has no "dev" channel -- so
    # web's VersionCheck.vue does channels["dev"].current_version on undefined
    # and throws on every page load. v7.5.0 is what the "rolling" channel
    # currently points at.
    "-X"
    "github.com/opencloud-eu/opencloud/pkg/version.Edition=rolling"
  ];

  tags = [ "enable_vips" ];

  nativeBuildInputs = [
    ncurses
    gettext
    pigeon
    go-mockery
    protoc-go-inject-tag
    pkg-config
  ];

  buildInputs = [
    libxcrypt
    vips
  ];

  # wants testcontainers and docker, and we don't have a good way to skip tests
  # based on package name and not test name
  preCheck = ''
    rm services/search/pkg/opensearch/*_test.go
  '';

  env = {
    # avoids 'make generate' calling `git`, otherwise no-op
    STRING = finalAttrs.version;
    VERSION = finalAttrs.version;
    # avoids weird test failure
    AUTOMEMLIMIT = "off";
  };

  excludedPackages = [ "tests/*" ];

  passthru = {
    web = callPackage ./web.nix { };
    idp-web = callPackage ./idp-web.nix { };
    tests = { inherit (nixosTests) opencloud; };
    updateScript = nix-update-script { };
  };

  doInstallCheck = true;
  nativeInstallCheckInputs = [ versionCheckHook ];
  versionCheckProgramArg = [ "version" ];

  meta = {
    description = "OpenCloud gives you a secure and private way to store, access, and share your files";
    homepage = "https://github.com/opencloud-eu/opencloud";
    changelog = "https://github.com/opencloud-eu/opencloud/blob/${finalAttrs.src.tag}/CHANGELOG.md";
    license = lib.licenses.asl20;
    maintainers = with lib.maintainers; [
      christoph-heiss
      k900
    ];
    mainProgram = "opencloud";
  };
})
