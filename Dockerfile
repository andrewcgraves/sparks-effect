FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/sparks-effect-routing-worker ./cmd/worker

FROM alpine:3.20

RUN apk add --no-cache ca-certificates && adduser -D -u 10001 appuser
COPY --from=build /out/sparks-effect-routing-worker /usr/local/bin/sparks-effect-routing-worker

USER appuser
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/sparks-effect-routing-worker"]
