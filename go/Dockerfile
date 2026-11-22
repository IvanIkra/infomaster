# The Go service (D81). Build context: the repository root, so the web client comes from infomaster/web/static:
#   docker build -f go/Dockerfile --build-arg APP_UID=$(id -u) -t infomaster:go .
# go/Dockerfile.dockerignore replaces the root .dockerignore (which lets in only the Python tree) for this build.

FROM golang:1.26 AS build
WORKDIR /src/go
# Modules first, so code changes don't download them again. Exact versions come from go.sum.
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY go/ ./
# No cgo: SQLite is modernc's pure Go, so the binary runs on any base image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infomaster ./cmd/infomaster \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/compare ./cmd/compare

FROM alpine:3.22
# tzdata: Moscow's zone for the logs and any library that asks the system (the binary embeds it too);
# ca-certificates: HTTPS to Anthropic, RUZ, HSE App X, Google Sheets and the rest.
RUN apk add --no-cache tzdata ca-certificates
WORKDIR /app
COPY infomaster/web/static infomaster/web/static
COPY --from=build /out/infomaster /out/compare /usr/local/bin/
ENV WEB_STATIC=/app/infomaster/web/static

# The service runs as a plain user (D79): a flaw in a parser does not run as root over the data directory. APP_UID
# is the owner of data/ on the host (.env), so its files stay the host user's; the code stays root's, read-only.
ARG APP_UID=1000
RUN addgroup -g "$APP_UID" app && adduser -D -H -u "$APP_UID" -G app -h /app/data -s /sbin/nologin app
USER app

# 8080: public (the web app and the connector, behind Funnel); 8081: internal (API for Tablo and HA)
EXPOSE 8080 8081
CMD ["infomaster", "--data", "data"]
