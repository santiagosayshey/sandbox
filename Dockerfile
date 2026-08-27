FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go generate ./...
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /sandboxd ./cmd/sandboxd

FROM gcr.io/distroless/static:nonroot@sha256:e754765ad9e167b0677b41c617fd44afb7b9818a477f48f17bda08e12cfb98cb
COPY --from=build /sandboxd /sandboxd
EXPOSE 8080
ENTRYPOINT ["/sandboxd"]
