# Smart Campaign Composer - frontend image (ADR 011).
#
#   docker build ./frontend                 # nginx serving the Vite build (default target)
#   docker build --target test ./frontend   # runs the Vitest suite inside the build
#
# nginx serves the static SPA and reverse-proxies /api and /healthz to the Go
# API, so the browser talks to a single origin (no CORS) exactly like the
# Vite dev proxy does in development.

ARG NODE_VERSION=22
ARG NGINX_VERSION=1.27

# -------------------------------------------------------------- npm install --
FROM node:${NODE_VERSION}-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json ./
# The lockfile pins the linux-musl native bindings of rolldown, lightningcss
# and @tailwindcss/oxide next to the Windows ones, so `npm ci` is reproducible
# in this Alpine image.
RUN npm ci --no-audit --no-fund

# -------------------------------------------------------------------- tests --
FROM deps AS test
COPY . .
RUN npm test

# -------------------------------------------------------------------- build --
FROM deps AS build
COPY . .
# Empty = same origin (/api is proxied by nginx below). Set only when the
# bundle is served without this proxy.
ARG VITE_API_BASE=
ENV VITE_API_BASE=${VITE_API_BASE}
RUN npm run build

# ------------------------------------------------------------------ runtime --
FROM nginx:${NGINX_VERSION}-alpine AS runtime
# The official image renders /etc/nginx/templates/*.template with envsubst at
# start-up (only variables that are set are replaced; nginx's own $vars stay).
ENV API_UPSTREAM=backend:8080 \
    NGINX_RESOLVER=127.0.0.11
COPY nginx/default.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
