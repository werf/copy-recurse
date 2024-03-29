.PHONY: fmt lint test

all: fmt lint test

fmt:
	gci -w --local github.com/werf/ .
	gofumpt -w .

lint:
	golangci-lint run ./...

test:
	ginkgo run -r -p $(ARGS) .
