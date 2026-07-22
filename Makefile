.PHONY: fmt tidy outdated update test test-debug vet staticcheck vuln build-nocgo check

fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

tidy:
	go mod tidy -diff

# Fail if any direct dependency or declared tool has a newer version.
outdated:
	@echo "Checking for module updates..."
	@out=$$( { \
		go list -m -u -f '{{if and .Update (not .Indirect)}}{{.Path}}: {{.Version}} -> {{.Update.Version}}{{end}}' all; \
		go list -m -u -f '{{if .Update}}{{.Path}}: {{.Version}} -> {{.Update.Version}}{{end}}' \
			honnef.co/go/tools golang.org/x/vuln; \
	} | sed '/^$$/d' | sort -u ); \
	if [ -z "$$out" ]; then \
		echo "All direct modules and tools are up to date."; \
	else \
		echo "$$out"; \
		echo ""; \
		echo "Run 'make update' to upgrade."; \
		exit 1; \
	fi

# Upgrade direct deps + declared tools to newest minor/patch, then tidy.
update:
	go get -u ./...
	go get -u tool
	go mod tidy

test:
	go test -race ./...

# Packages with //go:build debug code (view-drift, profiling).
test-debug:
	go test -race -tags debug ./internal/agent/ ./internal/profiling/

vet:
	go vet ./...

staticcheck:
	go tool staticcheck ./...

vuln:
	go tool govulncheck ./...

# Verifies the documented no-tree-sitter build path still compiles.
build-nocgo:
	CGO_ENABLED=0 go build -o gogen-nocgo .

# Local full check (sequential even under make -j).
# Does not auto-update deps — use 'make update' for that.
check:
	$(MAKE) -j1 fmt tidy test test-debug vet staticcheck vuln build-nocgo
