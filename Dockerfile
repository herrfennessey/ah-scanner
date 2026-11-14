# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS=linux TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ahc ./cmd/ahc

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ahc /ahc
ENV AHC_CONFIG=/config.yaml
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=5m --timeout=10s --start-period=10m CMD ["/ahc", "health"]
ENTRYPOINT ["/ahc"]
CMD ["collect"]
