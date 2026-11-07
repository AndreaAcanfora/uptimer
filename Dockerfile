# Build a static binary (pure-Go SQLite, no cgo), then ship it on a minimal image.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/uptimer ./cmd/uptimer

# distroless/static has CA certificates and timezone data, runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/uptimer /usr/local/bin/uptimer
WORKDIR /data
EXPOSE 8080
ENV UPTIMER_CONFIG=/data/uptimer.yml
ENTRYPOINT ["uptimer"]
CMD ["run"]
