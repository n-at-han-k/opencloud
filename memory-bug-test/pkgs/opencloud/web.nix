{
  lib,
  stdenvNoCC,
  fetchFromGitHub,
  nodejs,
  pnpm_11,
  fetchPnpmDeps,
  pnpmConfigHook,
  nix-update-script,
}:
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "opencloud-web";
  version = "7.4.0";

  src = fetchFromGitHub {
    owner = "opencloud-eu";
    repo = "web";
    tag = "v${finalAttrs.version}";
    hash = "sha256-idoR5R97k5ZfYBh7vboXzg2CdbVX5TBPrpL6s1k9qPA=";
  };

  pnpmDeps = fetchPnpmDeps {
    inherit (finalAttrs) pname version src;
    pnpm = pnpm_11;
    fetcherVersion = 3;
    hash = "sha256-zYtP1BxgNadKu/dhiQnsehTyrFJEj9aDGE3P+T9ErDY=";
  };

  nativeBuildInputs = [
    nodejs
    pnpmConfigHook
    pnpm_11
  ];

  buildPhase = ''
    runHook preBuild

    # Whitelabel: OpenCloud -> TradePortal in the web UI source (see whitelabel-web.sed).
    find packages -type f \( -name '*.ts' -o -name '*.vue' -o -name '*.js' -o -name '*.html' -o -name '*.css' -o -name '*.md' \) -print0 | xargs -0 sed -i -f ${./whitelabel-web.sed}
    sed -i -f ${./whitelabel-web.sed} vite.config.ts index.html

    pnpm build
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir $out
    cp -r dist/* $out
    runHook postInstall
  '';

  passthru.updateScript = nix-update-script { };

  meta = {
    description = "Web UI for OpenCloud built with Vue.js and TypeScript";
    homepage = "https://github.com/opencloud-eu/web";
    changelog = "https://github.com/opencloud-eu/web/blob/${finalAttrs.src.tag}/CHANGELOG.md";
    license = lib.licenses.agpl3Only;
    maintainers = with lib.maintainers; [
      christoph-heiss
      k900
    ];
    platforms = lib.platforms.all;
  };
})
