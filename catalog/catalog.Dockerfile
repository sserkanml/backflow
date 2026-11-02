# The file-based catalog image: opm serves the configs in catalog/out (make catalog), pre-built into a cache.
# Build with the directory catalog/ as the context: docker build -f catalog/catalog.Dockerfile catalog
FROM quay.io/operator-framework/opm:v1.74.0
ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]
ADD out /configs
RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]
LABEL operators.operatorframework.io.index.configs.v1=/configs
