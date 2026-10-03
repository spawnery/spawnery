# The documentation site's image. Takes only the identity from oci-common.
# Caddy initialises a data directory even with automatic HTTPS off, so the
# Deployment points XDG_DATA_HOME and XDG_CONFIG_HOME at an emptyDir.
{ dockerTools
, writeTextDir
, caddy
, docs-site
, oci-common
, runCommand
}:

let
  caddyfile = writeTextDir "etc/caddy/Caddyfile" ''
    {
      auto_https off
      admin off
    }
    :8080 {
      root * /site
      encode gzip
      file_server
      handle_errors {
        rewrite * /404.html
        file_server
      }
    }
  '';

  site = runCommand "docs-site-tree" { } ''
    mkdir -p $out/site
    cp -r ${docs-site}/. $out/site/
  '';
in
dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/docs";
  tag = "dev";

  # A label, not a cross-compile; see nix/oci-common.nix.
  architecture = "amd64";

  contents = [
    oci-common.passwd
    oci-common.group
    caddyfile
    site
    (oci-common.binIn { package = caddy; name = "caddy"; })
  ];

  config = {
    User = "${toString oci-common.uid}:${toString oci-common.gid}";
    WorkingDir = "/";
    Entrypoint = [ "/usr/local/bin/caddy" "run" "--config" "/etc/caddy/Caddyfile" ];
    Env = [ "XDG_DATA_HOME=/tmp" "XDG_CONFIG_HOME=/tmp" ];
    ExposedPorts = { "8080/tcp" = { }; };
    Labels = {
      "org.opencontainers.image.title" = "Spawnery documentation";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
    };
  };
}
