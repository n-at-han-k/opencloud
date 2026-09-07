{
  stdenvNoCC,
  lib,
  opencloud,
  pnpm_11,
  fetchPnpmDeps,
  pnpmConfigHook,
  nodejs,
}:
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "opencloud-idp-web";

  inherit (opencloud) src version;

  pnpmRoot = "services/idp";

  pnpmDeps = fetchPnpmDeps {
    inherit (finalAttrs) pname version src;
    pnpm = pnpm_11;
    sourceRoot = "${finalAttrs.src.name}/${finalAttrs.pnpmRoot}";
    fetcherVersion = 3;
    hash = "sha256-wX2q/updCsZTrPmzBNOdGamjiU+roDliAqyxQSXISFU=";
  };

  nativeBuildInputs = [
    nodejs
    pnpmConfigHook
    pnpm_11
  ];

  buildPhase = ''
    runHook preBuild

    cd $pnpmRoot

    # Whitelabel: OpenCloud -> TradePortal in the IDP web UI, and rename the
    # files whose names the rewritten source now refers to -- the React context
    # (its import path tracks the filename) and the two brand fonts, which
    # app.css loads by name from ./fonts.
    find src public i18n -type f \( -name '*.jsx' -o -name '*.js' -o -name '*.html' -o -name '*.css' -o -name '*.pot' \) -print0 | xargs -0 sed -i -f ${./whitelabel.sed}
    mv src/openCloudContext.js src/tradePortalContext.js
    mv src/fonts/OpenCloud500-Regular.woff2 src/fonts/TradePortal500-Regular.woff2
    mv src/fonts/OpenCloud750-Bold.woff2 src/fonts/TradePortal750-Bold.woff2

    pnpm build
    mkdir -p assets/identifier/static
    cp -v src/images/favicon.svg assets/identifier/static/favicon.svg
    cp -v src/images/icon-lilac.svg assets/identifier/static/icon-lilac.svg

    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall

    mkdir $out
    cp -r assets $out

    runHook postInstall
  '';

  meta = {
    description = "OpenCloud - IDP Web UI";
    homepage = "https://github.com/opencloud-eu/opencloud";
    changelog = "https://github.com/opencloud-eu/opencloud/blob/${finalAttrs.src.tag}/CHANGELOG.md";
    license = lib.licenses.asl20;
    maintainers = with lib.maintainers; [
      christoph-heiss
      k900
    ];
    platforms = lib.platforms.all;
  };
})
