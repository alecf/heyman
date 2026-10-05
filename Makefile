.PHONY: build test clean install fmt lint vet

BINARY_NAME := heyman
BUILD_DIR := bin
CMD_PATH := ./cmd/heyman

# Build flags
LDFLAGS := -s -w

build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_PATH)

test:
	go test -v ./...

clean:
	rm -rf $(BUILD_DIR)
	go clean

install:
	go install $(CMD_PATH)

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: vet
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed" && exit 1)
	golangci-lint run

# Run the binary
run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

# Build for all common platforms
build-all:
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(CMD_PATH)
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(CMD_PATH)
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(CMD_PATH)
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(CMD_PATH)
