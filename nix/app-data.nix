# Omahab native data layout — single source of truth for canonical paths.
#
# Parameterized by the evaluated NixOS `config` plus the `stateDir`/`dataDir`
# roots so storage generation and the installed catalog share one layout.
# All paths are canonical service roots, never disk-specific physical paths.
# Rebuildable caches/layers/model weights are not user data and are omitted,
# except where a unit's StateDirectory holds durable config (noted inline).
{ config, stateDir, dataDir }:

let
  get = attr: fallback:
    let v = builtins.tryEval attr;
    in if v.success then v.value else fallback;
in
{
  caddy = {
    data = [
      { name = "state"; path = "${stateDir}/caddy"; }
      { name = "config"; path = get config.services.caddy.dataDir "/var/lib/caddy"; }
    ];
    databases = [ ];
    quiesce_units = [ ];
  };
  pocket-id = {
    data = [
      { name = "state"; path = get config.services.pocket-id.dataDir "/var/lib/pocket-id"; }
    ];
    databases = [ ];
    quiesce_units = [ "pocket-id.service" ];
  };
  forgejo = {
    data = [
      { name = "state"; path = get config.services.forgejo.stateDir "/var/lib/forgejo"; }
    ];
    databases = [ "forgejo" ];
    quiesce_units = [ "forgejo.service" ];
  };
  woodpecker = {
    # Durable records live in shared PostgreSQL; no invented file directory.
    data = [ ];
    databases = [ "woodpecker-server" ];
    quiesce_units = [ "woodpecker-server.service" "woodpecker-agent-docker.service" ];
  };
  immich = {
    data = [
      { name = "state"; path = "${dataDir}/apps/immich"; }
    ];
    databases = [ "immich" ];
    quiesce_units = [ "immich-server.service" ];
  };
  paperless-ngx = {
    data = [
      { name = "state"; path = get config.services.paperless.dataDir "${dataDir}/apps/paperless"; }
    ];
    databases = [ "paperless" ];
    quiesce_units = [ "paperless-web.service" "paperless-consumer.service" "paperless-scheduler.service" "paperless-task-queue.service" ];
  };
  karakeep = {
    data = [
      { name = "state"; path = "/var/lib/karakeep"; }
      { name = "config"; path = get config.services.meilisearch.settings.db_path "/var/lib/meilisearch"; }
    ];
    databases = [ ];
    quiesce_units = [ "karakeep-web.service" "karakeep-workers.service" "meilisearch.service" ];
  };
  syncthing = {
    data = [
      { name = "state"; path = get config.services.syncthing.dataDir "${dataDir}/sync"; }
      { name = "config"; path = get config.services.syncthing.configDir "${dataDir}/sync/.config/syncthing"; }
    ];
    databases = [ ];
    quiesce_units = [ "syncthing.service" ];
  };
  litellm = {
    data = [
      { name = "state"; path = "${dataDir}/apps/litellm"; }
    ];
    databases = [ "litellm" ];
    quiesce_units = [ "litellm.service" ];
  };
  hermes = {
    data = [
      { name = "state"; path = "${stateDir}/hermes"; }
    ];
    databases = [ ];
    quiesce_units = [ "docker-hermes.service" ];
  };
  ntfy = {
    data = [
      { name = "state"; path = "/var/lib/ntfy-sh"; }
    ];
    databases = [ ];
    quiesce_units = [ "ntfy-sh.service" ];
  };
  embedding-worker = {
    data = [ ];
    databases = [ ];
    quiesce_units = [ ];
  };
  jellyfin = {
    data = [
      { name = "state"; path = "${dataDir}/apps/jellyfin"; }
    ];
    databases = [ ];
    quiesce_units = [ "jellyfin.service" ];
  };
  technitium = {
    data = [
      { name = "state"; path = "/var/lib/technitium-dns-server"; }
    ];
    databases = [ ];
    quiesce_units = [ "technitium-dns-server.service" ];
  };
  restic-server = {
    data = [
      { name = "state"; path = "${dataDir}/machine-backups"; }
    ];
    databases = [ ];
    quiesce_units = [ ];
  };
  postgresql = {
    data = [
      { name = "state"; path = get config.services.postgresql.dataDir "/var/lib/postgresql"; }
    ];
    databases = [ "forgejo" "woodpecker-server" "immich" "paperless" "litellm" ];
    quiesce_units = [ ];
  };
  server-data = {
    data = [
      { name = "state"; path = "${dataDir}/projects"; }
    ];
    databases = [ ];
    quiesce_units = [ ];
  };
}
