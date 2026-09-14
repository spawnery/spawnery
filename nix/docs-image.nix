# The documentation site's image: a static tree and something to serve it.
#
# Like the operator image and unlike the two game images, this takes from
# oci-common only the identity -- so all images run as the same uid and
# runAsNonRoot has a passwd entry to resolve -- and builds its own frame.
#
# The server writes nothing. Caddy nevertheless initialises a data directory
# at startup even with automatic HTTPS off, so the Deployment gives it an
# emptyDir at /tmp and points XDG_DATA_HOME and XDG_CONFIG_HOME there; that
# is what lets the root filesystem stay read-only.
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

  # A label, not a cross-compile, exactly as in nix/oci-common.nix.
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
