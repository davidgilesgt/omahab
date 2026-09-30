# Embedding worker — in-repo Python, own hardened unit.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
in
{
  config = mkIf config.services.omahab.enable {
    systemd.services.omahab-embedding-worker = {
      description = "Omahab embedding worker (loopback/UDS)";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [
        "network.target"
        "omahab-embedding-init.service"
      ];
      requires = [ "omahab-embedding-init.service" ];
      wantedBy = [ "multi-user.target" ];
      # The worker starts without a model until setup records a choice.
      # Every choice stages the English memory model. Worldwide document
      # search also stages its own model; full-text document search does not.
      # After config changes, omahabd restarts this unit so /health reflects
      # the loaded aliases.
      serviceConfig = {
        Type = "simple";
        ExecStart = "${config.services.omahab.embeddingWorkerPackage}/bin/omahab-embedding-worker --config /var/lib/omahab/embedding/pinned_models.json --transport uds --socket /run/omahab-embedding/embedding.sock";
        User = "omahab-embedding";
        Group = "omahab-embedding";
        RuntimeDirectory = "omahab-embedding";
        RuntimeDirectoryMode = "0750";
        Restart = "on-failure";
        RestartSec = 5;
        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ReadWritePaths = [ "/run/omahab-embedding" ];
        # Keep the config and verified model artifacts read-only in the
        # worker sandbox. Setup stages them before restarting this unit.
        ReadOnlyPaths = [ "/var/lib/omahab/embedding/pinned_models.json" "/var/lib/omahab/models" ];
        StateDirectory = "omahab-embedding";
      };
    };
    users.users.omahab-embedding = {
      isSystemUser = true;
      group = "omahab-embedding";
    };
    users.groups.omahab-embedding = { };

    # Root-prepared embedding config: the worker runs as its own user and
    # cannot create /var/lib/omahab subtrees. omahab-embedding-init writes
    # the empty default; omahabd later replaces it with the chosen model.
    systemd.services.omahab-embedding-init = {
      description = "Prepare Omahab embedding worker config";
      before = [ "omahab-embedding-worker.service" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = pkgs.writeShellScript "omahab-embedding-init" ''
          mkdir -p /var/lib/omahab/embedding
          chown root:omahab-embedding /var/lib/omahab/embedding
          chmod 0750 /var/lib/omahab/embedding
          # Pinned model artifacts live here; the worker reads them read-only
          # (ReadOnlyPaths above). Created up front so a setup-chosen pin has
          # a stable home before omahabd rewrites the config and restarts.
          mkdir -p /var/lib/omahab/models
          chown root:omahab-embedding /var/lib/omahab/models
          chmod 0750 /var/lib/omahab/models
          if [ ! -s /var/lib/omahab/embedding/pinned_models.json ]; then
            printf '%s' '{"models":{},"models_base_dir":"/var/lib/omahab/models","allow_test_adapter":false}' > /var/lib/omahab/embedding/pinned_models.json
            chown root:omahab-embedding /var/lib/omahab/embedding/pinned_models.json
            chmod 0640 /var/lib/omahab/embedding/pinned_models.json
          fi
        '';
      };
    };
  };
}
