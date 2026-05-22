FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/operator ./cmd/operator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/operator /operator
USER nonroot:nonroot
ENTRYPOINT ["/operator"]
