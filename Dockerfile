# Multi-stage build for BasalDB: static binaries in a minimal runtime image.
FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/basald ./cmd/basald \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/basal  ./cmd/basal

FROM alpine:3.19
RUN adduser -D -u 10001 basal
COPY --from=build /out/basald /usr/local/bin/basald
COPY --from=build /out/basal  /usr/local/bin/basal
USER basal
WORKDIR /data
# Listens on every interface, so the process refuses to start until
# BASAL_TOKEN is set (or you add -insecure-no-auth for a private network).
# Cloud Run injects PORT; pass no -addr and basald binds 0.0.0.0:$PORT.
# Commits are fsynced unless you pass -sync=false.
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["basald", "-file", "/data/basal.bsl", "-addr", "0.0.0.0:8080"]
