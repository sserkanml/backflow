# Backflow documentation

Backflow detects changes made directly in a cluster to resources that Argo CD manages, and proposes them back to Git as
merge requests. The [README](../README.md) says what it is and has a quick start; these pages are the rest.

## Start here

| Page | What it is |
|---|---|
| [Getting started](getting-started.md) | From the first install to the first merge request, step by step. |
| [Concepts](concepts.md) | The resources, how a drift becomes a merge request, the modes, the proposal lifecycle, verification by render, batching. |

## Install and configure

| Page | What it is |
|---|---|
| [Install with Helm](install/helm.md) | The chart and its values. |
| [Install on OpenShift](install/openshift.md) | OperatorHub (OLM), OpenShift GitOps with a local user, the service CA and the trusted CA bundle. |
| [Install with kustomize](install/kustomize.md) | Without Helm and without OLM. |
| [Configuration](configuration.md) | Every flag, its `BACKFLOW_*` variable and its Helm value; the allow lists. |

## Mapping a drift to Git

| Page | What it is |
|---|---|
| [What maps](mapping/overview.md) | What is mapped for each source type, and what stays `Unmapped`. |
| [Helm](mapping/helm.md) | Value files, chart defaults, maps and lists, labels and annotations. |
| [Kustomize](mapping/kustomize.md) | Overlays, bases, patches and settings. |
| [Charts from a Helm repository or a registry](mapping/multi-source.md) | Applications with several sources, scratch branches. |
| [Values set in the Application](mapping/application-values.md) | `helm.parameters`, `helm.values`, `kustomize` options: why they stay `Unmapped`. |
| [JSON files](mapping/json.md) | Manifests and value files that are JSON. |
| [Partial proposals](mapping/partial-proposals.md) | Proposing the part of a drift that can be proven. |

## Who changed it, and security

| Page | What it is |
|---|---|
| [Who changed it](who-changed-it.md) | The field manager from `managedFields`, the observe-only actor webhook, and what it keeps. |
| [Security](security.md) | Architecture and security model, permissions, Secret access, multi-tenancy and access rules. |
| [SECURITY.md](../SECURITY.md) | Reporting a vulnerability, supported versions, what counts. |
| [Compatibility and support](compatibility.md) | What the version number promises, and the support matrix. |

## Operate

| Page | What it is |
|---|---|
| [Metrics and alerts](operations/metrics.md) | The metrics, the alerts and what to do about them, the dashboard, PromQL examples. |
| [Upgrade and uninstall](operations/upgrade.md) | Upgrading the chart, the CRDs, and to 1.0. |
| [Upgrading to 1.0](operations/upgrade-1.0.md) | Moving objects to `backflow.io/v1` with `manager migrate`, for Helm, kustomize and OLM. |
| [Troubleshooting](operations/troubleshooting.md) | Every condition reason and every `Unmapped` reason: what it means and what to do. |

## Reference and development

| Page | What it is |
|---|---|
| [Limitations](reference/limitations.md) | What Backflow does not do, and the cases that surprise. |
| [Development](development.md) | Developing, testing, and running the end-to-end tests against GitLab and GitHub, locally and in CI. |
| [Design notes](design) | Why things are the way they are: API, multi-source mapping, Kustomize, JSON, Application values, the actor webhook. They are notes for developers, not user documentation. |
