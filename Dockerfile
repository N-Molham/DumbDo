# Stage 1: build the static Go binary (no network deps, stdlib only)
FROM golang:1.24-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY *.go ./

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dumbdo .

# Stage 2: tiny runtime (alpine provides busybox wget for the healthcheck
# and the shell tools the entrypoint needs when started as root)
FROM alpine:3.21

RUN apk add --no-cache su-exec

COPY --from=build /out/dumbdo /usr/local/bin/dumbdo
COPY public /app/public
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/docker-entrypoint.sh \
 && mkdir -p /app/data && chown 568:568 /app/data

WORKDIR /app

# Unraid/Synology standard uid/gid; the entrypoint chowns the data volume
# and drops privileges when the container is started as root
USER 568:568

EXPOSE 3000

HEALTHCHECK --interval=20s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:3000/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["dumbdo"]
