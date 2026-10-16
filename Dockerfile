# Multi-arch: the voting app runs on amd64 cloud nodes, the bridge on kind on
# an Apple Silicon laptop. Go cross-compiles natively, so the build stage runs
# on the builder's own platform instead of under emulation.
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /inference-vote .

FROM gcr.io/distroless/static-debian12
COPY --from=build /inference-vote /inference-vote
ENTRYPOINT ["/inference-vote"]
