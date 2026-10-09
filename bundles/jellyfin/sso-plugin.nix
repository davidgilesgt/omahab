{ lib, stdenv, buildDotnetModule, fetchzip, dotnetCorePackages }:

# Preserved Flowfin 4.3 source, before the 4.4 development line opened.
# sso-deps.json records the restored net9.0 dependency archive hashes.
buildDotnetModule {
  pname = "jellyfin-sso-plugin";
  version = "4.3.0";
  src = fetchzip {
    url = "https://codeload.github.com/zytoc/jellyfin-plugin-sso/tar.gz/cfee8680ceb04d46542dd22b6c3cfef4296a6ace";
    extension = "tar.gz";
    hash = "sha256-Nu9BILc+0BU66KX+7nSWtWun2B57BRIMMnHn2eYWZVM=";
  };
  projectFile = "SSO-Auth/SSO-Auth.csproj";
  nugetDeps = ./sso-deps.json;
  dotnet-sdk = dotnetCorePackages.sdk_9_0;
  dotnetFlags = [ "-p:TargetFrameworks=net9.0" ];
  # Mutation testing tools are not part of the installed plugin.
  postPatch = ''
    rm -f .config/dotnet-tools.json
  '';
  executables = [ ];
  installPhase = ''
    runHook preInstall
    dotnet publish SSO-Auth/SSO-Auth.csproj --no-build --no-restore \
      --configuration Release --framework net9.0 \
      --runtime ${dotnetCorePackages.systemToDotnetRid stdenv.hostPlatform.system} \
      -p:TargetFrameworks=net9.0 -p:SelfContained=false \
      --output "$TMPDIR/plugin-publish"
    mkdir -p "$out"
    for name in SSO-Auth Duende.IdentityModel.OidcClient Duende.IdentityModel \
      Microsoft.IdentityModel.JsonWebTokens Microsoft.IdentityModel.Tokens \
      Microsoft.IdentityModel.Logging Microsoft.IdentityModel.Abstractions \
      Microsoft.Bcl.Cryptography System.Security.Cryptography.Xml \
      System.Security.Cryptography.Pkcs System.Formats.Asn1; do
      cp "$TMPDIR/plugin-publish/$name.dll" "$out/"
    done
    cat > "$out/meta.json" <<'JSON'
    {"name":"Community SSO for Jellyfin","guid":"505ce9d1-d916-42fa-86ca-673ef241d7df","version":"4.3.0.0","targetAbi":"10.11.0.0","status":"Active","autoUpdate":false}
    JSON
    runHook postInstall
  '';
  meta = {
    description = "Pocket ID compatible SSO plugin for Jellyfin 10.11";
    license = lib.licenses.gpl3Only;
    platforms = [ "x86_64-linux" "aarch64-linux" ];
  };
}
