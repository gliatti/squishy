# syntax=docker/dockerfile:1.6

# --- dev : go run ---
FROM golang:1.25-alpine AS dev
# docker-cli + compose are needed by the dev-loop MCP tools (restart_api,
# run_unit_tests, get_step_payload). The host's docker socket is mounted in
# docker-compose.yml; this image only needs the client binaries.
RUN apk add --no-cache git build-base curl bash wget docker-cli docker-cli-compose
WORKDIR /app
ENV CGO_ENABLED=0
CMD ["go", "run", "."]

# --- build : binaire statique ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/squishy-mcp .

# --- runtime : image minimale ---
FROM gcr.io/distroless/static-debian12 AS prod
COPY --from=build /out/squishy-mcp /squishy-mcp
EXPOSE 8000
ENTRYPOINT ["/squishy-mcp"]
