# The Canvas base image: the Purpur image's shape over Canvas's jar. The pod
# spec is already written, so this image must provide
# /usr/local/bin/spawnery-slp, port 25565, working directory /data, scratch
# /tmp, a numeric user, and nothing else writable.
{ bash
, buildEnv
, coreutils
, findutils
, paper-jre
, runCommand
, unzip
, canvas
, spawnery-slp
, spawnery-config
, agents
, imageVersion
, oci-common
}:

let
  canvasHome = runCommand "canvas-home" { } ''
    mkdir -p $out/opt/canvas
    cp ${canvas.canvasJar} $out/opt/canvas/canvas.jar
    cp -r ${canvas.repo} $out/opt/canvas/repo
    chmod -R a-w $out/opt/canvas
  '';

  launch = import ./flat-launch.nix { inherit runCommand unzip; } {
    name = "canvas";
    serverJar = canvas.canvasJar;
    inherit (canvas) repo;
    home = "/opt/canvas";
  };

  agent = runCommand "canvas-agent-image-path" { } ''
    mkdir -p $out/opt/canvas/agent
    cp ${agents}/share/spawnery/paper/spawnery-agent.jar $out/opt/canvas/agent/spawnery-agent.jar
  '';
in
oci-common.layeredImage {
  name = "ghcr.io/spawnery/canvas";
  tag = "${canvas.canvasVersion}-${imageVersion}";

  contents = [
    (buildEnv {
      name = "canvas-tools";
      paths = [ bash coreutils findutils paper-jre ];
      pathsToLink = [ "/bin" ];
    })
    oci-common.passwd
    oci-common.group
    canvasHome
    launch
    agent
    (oci-common.binIn { package = spawnery-slp; name = "spawnery-slp"; })
    (oci-common.binIn { package = spawnery-config; name = "spawnery-config"; })
    (oci-common.entrypointFrom ../image/entrypoint.sh)
  ];

  config = {
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "SPAWNERY_PAPER_HOME=/opt/canvas"
      "SPAWNERY_CONFIG_FLAVOR=canvas"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Canvas base image";
      "org.opencontainers.image.version" = "${canvas.canvasVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      "cloud.spawnery.canvas-build" = canvas.canvasBuild;
    };
  };
}
