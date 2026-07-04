# Build the single static MacQuiz binary (API + worker modes).
# The same image runs as the `app` and `worker` Compose services;
# only the command differs (docs/09-deployment.md section 4).
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /macquiz ./cmd/macquiz

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /macquiz /macquiz
EXPOSE 8080
ENTRYPOINT ["/macquiz"]
CMD ["serve"]
