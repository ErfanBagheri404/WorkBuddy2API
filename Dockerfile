# syntax=docker/dockerfile:1

# Build a static binary so the runtime image needs no libc, shell or package
# manager. The build stage runs on the native platform and cross-compiles to
# TARGETARCH, so a multi-arch build does not need QEMU for this small program.
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.Version=v${VERSION}" \
    -o /out/workbuddy2api .

# gcr.io/distroless/static-debian12 includes CA certificates and runs as the
# nonroot user (uid 65532). No token or auth file is baked into the image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/workbuddy2api /usr/local/bin/workbuddy2api

# The proxy writes $HOME/.workbuddy2api-auth.json and refreshes it in place.
# Mount a writable volume at /auth and run the container with HOME=/auth.
ENV HOME=/auth
EXPOSE 61021

# No default command: `docker run -it <image>` opens the interactive login
# menu, and the server is started explicitly with `--headless`.
ENTRYPOINT ["/usr/local/bin/workbuddy2api"]
