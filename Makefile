BINARY := bin/tollgate

.PHONY: build test lint fmt run clean

build:
	CGO_ENABLED=0 go build -o $(BINARY) ./cmd/tollgate

test:
	go test ./...

lint:
	go vet ./...
	@fmtout="$$(gofmt -l .)"; if [ -n "$$fmtout" ]; then echo "gofmt needed:"; echo "$$fmtout"; exit 1; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not installed; skipping (CI runs it)"; fi

fmt:
	gofmt -w .

run: build
	./$(BINARY) --config config.yaml

clean:
	rm -rf bin
