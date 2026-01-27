# Build the manager binary
FROM golang:1.21 as builder

WORKDIR /workspace
# Copy only go.mod (go.sum will be generated inside the image)
COPY go.mod go.mod
# Download module dependencies based on go.mod to warm the module cache
RUN go env -w GOPROXY=https://proxy.golang.org,direct && go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Regenerate go.sum with full source context, verify, then build
RUN go mod tidy && go mod verify && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
