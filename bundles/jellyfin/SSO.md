# Jellyfin SSO dependency

Pocket ID SSO uses the preserved Flowfin 4.3 source at
`cfee8680ceb04d46542dd22b6c3cfef4296a6ace` in
`zytoc/jellyfin-plugin-sso`.
The original Flowfin release download returned HTTP 404 during fresh ISO installation.

The build selects `net9.0` for Jellyfin 10.11.
It keeps the original plugin GUID and `JELLYFIN_SSO_CONFIG_FILE` interface.
Only the runtime assemblies listed in the upstream `build.yaml` enter the plugin directory.
Jellyfin and shared framework assemblies must remain supplied by the host.

The source archive and NuGet archives have fixed hashes.
`sso-deps.json` records the dependency versions restored from the upstream project.
Use Nix's `fetch-deps` tool to regenerate archive hashes after a reviewed source update.
NuGet audit requires network access and is disabled in the isolated Nix build.
Review dependency advisories separately before updating this package.

For an update, verify the Jellyfin ABI, provider document, secret references, and account identity first.
Run the installed-closure build before a fresh ISO installation.
Then verify plugin loading and Pocket ID sign-in in the installed guest.
A successful compile does not establish working SSO.
