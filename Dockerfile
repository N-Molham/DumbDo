# Stage 1: pregenerate logo PNGs once at build time (removes sharp from the runtime image)
FROM node:20-alpine AS builder

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY scripts ./scripts
COPY public ./public
RUN node scripts/convert-logo.js

# Stage 2: Bun runtime (no sharp, no native deps, small image)
FROM oven/bun:1-alpine

WORKDIR /app

COPY --from=builder --chown=568:568 /app/public ./public
COPY --chown=568:568 server.js ./
COPY --chown=568:568 scripts ./scripts
COPY --chown=568:568 package.json ./

# Production deps only (express, cookie-parser, cors)
RUN bun install --production

# Data directory for the volume mount
RUN mkdir -p /app/data && chown 568:568 /app/data

COPY --chown=568:568 scripts/docker-entrypoint.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && apk add --no-cache su-exec

USER 568:568

EXPOSE 3000

# Healthcheck against the public /health endpoint (no wget package needed)
HEALTHCHECK --interval=20s --timeout=5s --start-period=10s --retries=3 \
    CMD bun -e "fetch('http://127.0.0.1:3000/health').then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"

# Start the application (entrypoint chowns the data volume when run as root,
# then drops privileges to the bun user)
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["bun", "server.js"]
