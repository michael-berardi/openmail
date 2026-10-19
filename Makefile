VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)

.PHONY: build test install demo clean app install-app
build:
	go build -trimpath -ldflags "-s -w -X github.com/michael-berardi/openmail/internal/core.Version=$(VERSION)" -o bin/openmail ./cmd/openmail
test:
	go vet ./... && go test -race ./...
install: build
	install -m 0755 bin/openmail $(HOME)/.local/bin/openmail
demo: build
	@d=$$(mktemp -d); echo "demo data in $$d"; ./bin/openmail demo --home $$d
clean:
	rm -rf bin dist

# macOS desktop app: native window on the system web view, same engine and UI.
app:
	./scripts/bundle-macos.sh
install-app: app
	rm -rf $(HOME)/Applications/OpenMail.app && mkdir -p $(HOME)/Applications && cp -R dist/OpenMail.app $(HOME)/Applications/OpenMail.app
