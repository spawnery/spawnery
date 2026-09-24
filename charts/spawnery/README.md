# spawnery

The Helm chart for the Spawnery operator. Since milestone 6d this is the only
way the operator installs — `config/deploy/`, the flat manifests this chart
replaces, no longer exists in this repository.

```bash
helm install spawnery oci://ghcr.io/spawnery/charts/spawnery \
  --version 0.6.0 --namespace spawnery-system --create-namespace
```

Installing the chart, choosing a game namespace, the values and the `/cloud`
permissions are all covered in [Getting started](https://docs.spawnery.cloud/getting-started/).
