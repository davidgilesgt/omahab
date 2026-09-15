# Shared pinned LiteLLM Python environment (single pin source).
#
# Both the native gateway service and the provider-auth helper run on this
# interpreter with these packages: litellm 1.97.0 (nixpkgs pin) with the
# fastapi 0.140.6 scope override (stock fastapi removed the private helper
# litellm imports), the a2a test gate, and the prebuilt Prisma client tree.
# Importing this file (not duplicating the override) is what keeps the two
# environments identical.
{ pkgs }:

let
  # LiteLLM stays on Python 3.13 while the default is 3.14: the prebuilt
  # Prisma client is a 25MB types.py (~77k TypedDicts), and on 3.14 every
  # TypedDict creation goes through typing_extensions'
  # annotationlib.call_annotate_function (~5 min of 100% CPU per import,
  # paid 3+ times per startup by the server and its `prisma` children),
  # so :4000 never opens and the app reports unhealthy.
  litellmPython = pkgs.python313;
  litellmPyPackages = pkgs.python313Packages;
  # Prisma engines matching prisma-client-py 0.15.0 (expects CLI 5.17.0,
  # engines commit 393aa359). nixpkgs only ships engines_6/_7, whose
  # schema-engine speaks a newer schemaPush protocol (missing field
  # `filters`), so `prisma db push` fails and fresh installs never get
  # tables. Prebuilt upstream binaries for debian-openssl-3.0.x — the same
  # files client-py downloads at runtime — patchelf'd for NixOS.
  litellmPrismaEngines =
    let
      commit = "393aa359c9ad4a4bb28630fb5613f9c281cde053";
      platform = "debian-openssl-3.0.x";
      base = "https://binaries.prisma.sh/all_commits/${commit}/${platform}";
      fetchEngine = name: sha256: pkgs.fetchurl { url = "${base}/${name}.gz"; inherit sha256; };
      engines = {
        query-engine = fetchEngine "query-engine" "9bc857debe0d5760c571ee6271076223d94be981c29a22401de681f3f70e067d";
        schema-engine = fetchEngine "schema-engine" "98ad433fd64da2ea1eb6d565553a9a4339ecf30f1c211b28b69690f591269a41";
        prisma-fmt = fetchEngine "prisma-fmt" "e6bf23db1e7fe456ae167f4744a71dfad9075727ce6d0f65582a5bee5fe8793f";
        libquery_engine = fetchEngine "libquery_engine.so.node" "125d75739bd7fcdb8eabb5428355b5be00f540043e6bc1f5b30268947afab1bd";
      };
    in
    pkgs.stdenv.mkDerivation {
      name = "litellm-prisma-engines-5.17.0";
      nativeBuildInputs = [ pkgs.autoPatchelfHook ];
      buildInputs = [ pkgs.openssl pkgs.zlib pkgs.stdenv.cc.cc.lib ];
      buildCommand = ''
        mkdir -p $out/bin $out/lib
        gunzip -c ${engines."query-engine"} > $out/bin/query-engine
        gunzip -c ${engines."schema-engine"} > $out/bin/schema-engine
        gunzip -c ${engines."prisma-fmt"} > $out/bin/prisma-fmt
        chmod +x $out/bin/query-engine $out/bin/schema-engine $out/bin/prisma-fmt
        gunzip -c ${engines.libquery_engine} > $out/lib/libquery_engine.node
        autoPatchelf $out
      '';
    };
  # Prebuilt Prisma client for litellm's schema. prisma-client-py shells
  # out to the Node Prisma CLI (nodeenv + engine downloads) during
  # generate, so this runs with network in a fixed-output derivation; the
  # hashed tree is copied into the prisma package in the litellm python
  # override below and the final closure stays hermetic. Bump the name +
  # hash with litellm.
  litellmPrismaSchema = "${litellmPyPackages.litellm}/${litellmPython.sitePackages}/litellm/proxy/schema.prisma";
  litellmPrismaClient = pkgs.stdenv.mkDerivation {
    name = "litellm-prisma-client-1.97.0";
    outputHashAlgo = "sha256";
    outputHashMode = "recursive";
    outputHash = "sha256-SG//W6x30RLYhXG7I/HBT83cExB4GrGMQuO/uqlonGA=";
    nativeBuildInputs = [
      pkgs.cacert
      pkgs.nodejs
      pkgs.prisma-engines_6
      (litellmPython.withPackages (ps: [ ps.prisma ]))
    ];
    buildCommand = ''
      # Fixed-output sandboxes start without $out: create it (the
      # generator mkdirs it too, but copy_tree inherits the read-only
      # store mode onto it, so own it first while it is still empty).
      # The vendored query-engine paths must NOT leak into the output
      # (fixed-output results cannot reference store paths): unset them
      # so the baked BINARY_PATHS stay cache-local. The unit sets them
      # at runtime, which the client prefers over baked paths.
      unset PRISMA_QUERY_ENGINE_BINARY PRISMA_QUERY_ENGINE_LIBRARY
      mkdir -p $out
      export SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
      export HOME=$TMPDIR/home
      mkdir -p $HOME
      export PRISMA_HOME_DIR=$TMPDIR/prisma-home
      mkdir -p $PRISMA_HOME_DIR
      # The generator copy_trees its base package onto the output dir,
      # inheriting the read-only store mode, so run it from a writable
      # copy of the prisma package (3MB) with the env behind it.
      PRISMA_PKG=$(python -c "import prisma, os; print(os.path.dirname(prisma.__file__))")
      mkdir -p $TMPDIR/pkgs
      mkdir -p $TMPDIR/pkgs/prisma
      tar -C $PRISMA_PKG -cf - . | tar -C $TMPDIR/pkgs/prisma -xf -
      chmod -R u+w $TMPDIR/pkgs/prisma
      export PYTHONPATH=$TMPDIR/pkgs:$PYTHONPATH
      cp ${litellmPrismaSchema} $TMPDIR/schema.prisma
      chmod +w $TMPDIR/schema.prisma
      # Point the generator at $out: client-py copies its base tree there
      # and renders the models, yielding a complete `prisma` package.
      sed -i '/provider = "prisma-client-py"/a\  output = "'$out'"' $TMPDIR/schema.prisma
      (cd $TMPDIR && python -m prisma generate --schema ./schema.prisma)
      # The packaged schema copy carries the absolute output path used for
      # generation; a fixed-output result must not reference store paths.
      sed -i '\|output *= *"/nix/store/|d' $out/schema.prisma
    '';
  };
  litellmScope = litellmPyPackages.overrideScope (
    _self: super: {
      fastapi = super.fastapi.overridePythonAttrs (_old: rec {
        version = "0.140.6";
        src = pkgs.fetchFromGitHub {
          owner = "tiangolo";
          repo = "fastapi";
          tag = version;
          hash = "sha256-ZWLXNHmM2uGE5y7hLYW4zNSeCGo9d9X2KNT70aXK6W0=";
        };
      });
      # a2a-sdk 0.3.26: its suite runs at build time via
      # installCheckPhase (doInstallCheck defaults true), and one
      # telemetry assertion fails on Linux in this pin (seen on 3.14, kept for 3.13):
      # tests/utils/test_telemetry.py::
      # test_trace_function_sync_attribute_extractor_error_logged.
      # Same test is already disabled on Darwin upstream, so this
      # extends that lack of trust to Linux — the other ~820 tests
      # still run and gate the build.
      a2a-sdk = super.a2a-sdk.overridePythonAttrs (old: {
        disabledTests = (old.disabledTests or [ ]) ++ [
          "test_trace_function_sync_attribute_extractor_error_logged"
        ];
      });
      prisma = super.prisma.overridePythonAttrs (oldPrisma: {
        # Native DB ownership needs `from prisma import Prisma` at
        # gateway startup. Copy in the prebuilt client tree (built
        # with network in litellmPrismaClient below); this step is a
        # pure store-to-store copy, then assert the import resolves.
        postInstall = (oldPrisma.postInstall or "") + ''
          cp -r ${litellmPrismaClient}/. $out/${super.python.sitePackages}/prisma/
          PYTHONPATH=$out/${super.python.sitePackages}:$PYTHONPATH python -c "from prisma import Prisma"
        '';
      });
    }
  );
in
{
  inherit litellmPython litellmPyPackages litellmPrismaEngines litellmPrismaClient litellmScope;
  # The gateway package with the pinned scope.
  litellmPackage = pkgs.litellm.override { python3Packages = litellmScope; };
  # Interpreter carrying the pinned scope for the provider-auth helper.
  providerPython = litellmPython.withPackages (ps: [ litellmScope.litellm ]);
}
