FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/sparks-effect-routing-worker ./cmd/worker

FROM alpine:3.20

RUN adduser -D -u 10001 appuser
COPY --from=build /out/sparks-effect-routing-worker /usr/local/bin/sparks-effect-routing-worker

# No EXPOSE: this is a queue consumer, not a server. It dials out to the broker,
# Postgres and Valhalla, and nothing ever connects to it.
USER appuser
ENTRYPOINT ["/usr/local/bin/sparks-effect-routing-worker"]
