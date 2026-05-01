# syntax=docker/dockerfile:1.6

# --- dev : go run + go test (alpine, CGO ok via build-base for race detector) ---
# CGO is enabled here because `go test -race` needs it. The build-base apk
# package provides gcc/musl-dev. The static prod binary is built in the next
# stage which sets CGO_ENABLED=0 explicitly.
FROM golang:1.25-alpine AS dev
RUN apk add --no-cache git build-base curl bash wget
WORKDIR /app
CMD ["go", "run", "./cmd/squishy"]

# --- dev-db2 : alternate dev image with IBM DB2 clidriver bundled ---
# go_ibm_db (the only viable Go DB2 driver) is CGO + needs IBM's clidriver
# library for libdb2.so at link/run time. clidriver only ships glibc binaries,
# so this stage uses Debian bookworm-slim instead of alpine. Pulled at the
# cost of ~80 MB extra image size; only the api service targets this stage
# (docker-compose.yml). Unit tests and pure-Go e2e stay on the alpine `dev`
# target.
#
# Build prerequisite at compile time: we set CGO_ENABLED=1 and pass `-tags db2`
# so the conditional import in internal/connection/db2_driver_cgo.go fires and
# registers the "go_ibm_db" sql/driver name. Without `-tags db2` the binary
# still compiles cleanly (the driver_stub.go fallback) but OpenDB2 returns
# "sql: unknown driver" at runtime — by design.
FROM golang:1.25-bookworm AS dev-db2
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl bash git wget \
        libxml2 libpam0g ksh \
 && rm -rf /var/lib/apt/lists/*
ENV IBM_DB_HOME=/opt/ibm/clidriver \
    LD_LIBRARY_PATH=/opt/ibm/clidriver/lib \
    CGO_ENABLED=1 \
    CGO_CFLAGS="-I/opt/ibm/clidriver/include" \
    CGO_LDFLAGS="-L/opt/ibm/clidriver/lib"
# clidriver: download IBM's linux x86_64 ODBC CLI tarball (~80 MB) directly
# from the public artifact store. The go_ibm_db `installer` CLI calls
# `tar -xzf <file> -C -force` which fails on modern tar; do it ourselves.
RUN mkdir -p ${IBM_DB_HOME} && \
    curl -fsSL -o /tmp/clidriver.tar.gz \
      https://public.dhe.ibm.com/ibmdl/export/pub/software/data/db2/drivers/odbc_cli/linuxx64_odbc_cli.tar.gz && \
    tar -xzf /tmp/clidriver.tar.gz -C /opt/ibm && \
    rm /tmp/clidriver.tar.gz
WORKDIR /app
# Runs the api with the db2 build tag so the IBM driver is registered.
CMD ["go", "run", "-tags", "db2", "./cmd/squishy"]

# --- build : binaire statique sans DB2 ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/squishy ./cmd/squishy

# --- runtime : image minimale (sans DB2 — usage prod sans sources DB2) ---
FROM gcr.io/distroless/static-debian12 AS prod
COPY --from=build /out/squishy /squishy
ENTRYPOINT ["/squishy"]

# --- prod-db2 : runtime debian + clidriver, binaire compilé avec -tags db2 ---
# Distroless ne fournit pas glibc + libdb2 ; on utilise debian-slim pour
# obtenir un runtime compatible CGO. Pour les déploiements qui ne touchent
# jamais DB2, garder le target `prod` (distroless statique).
FROM golang:1.25-bookworm AS build-db2
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates libxml2 libpam0g \
 && rm -rf /var/lib/apt/lists/*
ENV IBM_DB_HOME=/opt/ibm/clidriver \
    LD_LIBRARY_PATH=/opt/ibm/clidriver/lib \
    CGO_ENABLED=1 \
    CGO_CFLAGS="-I/opt/ibm/clidriver/include" \
    CGO_LDFLAGS="-L/opt/ibm/clidriver/lib"
RUN go install github.com/ibmdb/go_ibm_db/installer@latest && \
    /go/bin/installer -force && \
    mkdir -p ${IBM_DB_HOME} && \
    cp -a /go/pkg/mod/github.com/ibmdb/go_ibm_db@*/installer/clidriver/. ${IBM_DB_HOME}/
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN go build -tags db2 -ldflags="-s -w" -o /out/squishy ./cmd/squishy

FROM debian:bookworm-slim AS prod-db2
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates libxml2 libpam0g \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build-db2 /opt/ibm/clidriver /opt/ibm/clidriver
COPY --from=build-db2 /out/squishy /squishy
ENV LD_LIBRARY_PATH=/opt/ibm/clidriver/lib \
    IBM_DB_HOME=/opt/ibm/clidriver
ENTRYPOINT ["/squishy"]
