# Vendored charts

Used only by `make coverage` (build tag `coverage`); nothing here is shipped.

| Chart | Version | Source | Licence |
|---|---|---|---|
| nginx | 25.2.8 | `oci://registry-1.docker.io/bitnamicharts/nginx` | Apache-2.0 (Bitnami, `annotations.licenses` in Chart.yaml) |
| redis | 28.3.1 | `oci://registry-1.docker.io/bitnamicharts/redis` | Apache-2.0 |
| postgresql | 18.12.14 | `oci://registry-1.docker.io/bitnamicharts/postgresql` | Apache-2.0 |
| webapp | 0.3.0 | this repository | same as the repository |
| kvstore | 0.1.0 | this repository, source in `testdata/subcharts/kvstore`, packaged into `webapp/charts/kvstore-0.1.0.tgz` with `helm package` (dependency alias `store`) | same as the repository |

The packages are the unmodified output of `helm pull`. Pin new versions here
and in `charts.go`.
