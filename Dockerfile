FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/fakegateway .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/fakegateway /usr/local/bin/fakegateway
USER nobody
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8090/healthz || exit 1
ENTRYPOINT ["fakegateway"]
