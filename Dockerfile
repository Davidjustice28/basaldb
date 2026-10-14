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
# Cloud Run injects PORT; basald also accepts -addr.
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["basald", "-file", "/data/basal.bsl", "-addr", ":8080"]
