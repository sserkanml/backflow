# Install with kustomize

Without Helm and without OLM, the manifests of the repository install Backflow. `config/default` has the CRDs, the RBAC, the
Deployment and the metrics Service, with the namespace `backflow-system` and the name prefix `backflow-`. The image
(`ghcr.io/sserkanml/backflow`, for linux/amd64 and linux/arm64) is set in `config/manager/kustomization.yaml`.

## Install

From a checkout:

```bash
make deploy IMG=ghcr.io/sserkanml/backflow:1.0.0      # kustomize build config/default | kubectl apply -f -
```

Or, from a kustomization of your own that you keep in Git (the better way, because the settings below go in the same place):

```yaml
# kustomization.yaml of your overlay
resources:
- github.com/sserkanml/backflow/config/default?ref=v1.0.0
```

`make build-installer IMG=...` writes one file with everything, `dist/install.yaml`. To install only the CRDs there is
`make install`, and `make uninstall` removes them (which deletes every Backflow object).

## Configure

There is no chart value: the settings are flags of the manager, or the `BACKFLOW_*` environment variables, set by patching the
Deployment of your overlay. **The defaults are the chart's**: the manifests set `BACKFLOW_SCM_ALLOWED_HOSTS=gitlab.com,github.com`,
so a self-hosted GitLab or GitHub has to be added. The table of every flag and variable, and the patches that set them (with the
caveat that a flag replaces the variable of the same setting), are in
[Configuration](../configuration.md#allowed-servers-and-hosts). The Argo CD connection, the allow lists and `defaultAccess` work as
they do with Helm.

## Upgrade

Apply the CRDs of the new version first, then change `ref` and apply: see [Upgrade and uninstall](../operations/upgrade.md).
