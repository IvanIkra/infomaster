# The service (D81): docker build --build-arg APP_UID=$(id -u) -t infomaster .
# Compiling modernc's SQLite takes ~2 GB of memory: on a small host build with scripts/deploy.sh instead, which
# compiles elsewhere and puts the same runtime image together from the binaries (Dockerfile.prebuilt).

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
COPY go/web/static web/static
COPY --from=build /out/infomaster /out/compare /usr/local/bin/
ENV WEB_STATIC=/app/web/static

# The service runs as a plain user (D79): a flaw in a parser does not run as root over the data directory. APP_UID
# is the owner of data/ on the host (.env), so its files stay the host user's; the code stays root's, read-only.
ARG APP_UID=1000
RUN addgroup -g "$APP_UID" app && adduser -D -H -u "$APP_UID" -G app -h /app/data -s /sbin/nologin app
USER app

# 8080: public (the web app and the connector, behind Funnel); 8081: internal (API for Tablo and HA)
EXPOSE 8080 8081
CMD ["infomaster", "--data", "data"]
