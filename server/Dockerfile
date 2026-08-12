# Build the single static MacQuiz binary (API + worker modes).
# The same image runs as the `app` and `worker` Compose services;
# only the command differs (docs/09-deployment.md section 4).
#
# The build stage is pinned to the *builder's* architecture and cross-compiles
# to the target's, so an x86 GitHub runner produces the arm64 image the
# Graviton host runs (docs/09-deployment.md section 9.2) at native speed and
# with no QEMU emulation layer. A plain `FROM golang` would instead run the
# whole toolchain under emulation, which needs docker/setup-qemu-action and
# is several times slower. Go makes this free: CGO_ENABLED=0 with GOARCH set
# is all a static binary needs.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
# Supplied automatically by buildx from the requested --platform.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /macquiz ./cmd/macquiz

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /macquiz /macquiz
EXPOSE 8080
ENTRYPOINT ["/macquiz"]
CMD ["serve"]
