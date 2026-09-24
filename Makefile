VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# MAP_API_KEY (the release pipeline's CARTO_API_KEY secret) is baked into the binary as the
# default map tile key; REPEATERTASTIC_MAP_API_KEY overrides it at run time. Recipes that use it
# are silent so the key isn't echoed.
MAP_API_KEY ?=
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.mapAPIKey=$(MAP_API_KEY)
GOFLAGS := -trimpath
DIST := dist

.PHONY: all build ui test race lint interop dist clean proto firmware plugin-example shim shim-test

all: build

build:
	@echo "building bin/repeatertastic bin/kisstool ($(VERSION))"
	@CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/repeatertastic ./cmd/repeatertastic
	@CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/kisstool ./cmd/kisstool

ui:
	cd ui && npm ci && npm run build

test:
	go vet ./...
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run ./...
	cd ui && npx vue-tsc --build

proto:
	./scripts/gen-proto.sh

# Static binaries for a Raspberry Pi (64-bit OS, 32-bit OS, Pi Zero/1) and x86-64.
dist:
	@mkdir -p $(DIST)
	@for t in linux/arm64 linux/arm/7 linux/arm/6 linux/amd64; do \
		os=$${t%%/*}; rest=$${t#*/}; arch=$${rest%%/*}; arm=$${rest#*/}; \
		suffix=$$arch; [ "$$arch" = arm ] && suffix=armv$$arm; \
		echo "building $$os-$$suffix"; \
		GOOS=$$os GOARCH=$$arch GOARM=$$( [ "$$arch" = arm ] && echo $$arm ) CGO_ENABLED=0 \
			go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/repeatertastic-$$os-$$suffix ./cmd/repeatertastic || exit 1; \
		GOOS=$$os GOARCH=$$arch GOARM=$$( [ "$$arch" = arm ] && echo $$arm ) CGO_ENABLED=0 \
			go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/kisstool-$$os-$$suffix ./cmd/kisstool || exit 1; \
	done
	@cd $(DIST) && sha256sum repeatertastic-* kisstool-* > SHA256SUMS

firmware:
	./firmware/build.sh

# The LD_PRELOAD I2C shim that makes a stock meshtasticd believe it owns a sensor
# (shim/README.md). Built inside debian:trixie for amd64, arm64 and arm (armv7
# hard-float), named for the GOARCH each one is embedded under. No armv6: a Pi
# Zero or Pi 1 gets no shim and the daemon says so.
shim:
	./shim/build.sh
	@cp shim/build/i2cshim-linux-amd64.so shim/build/i2cshim-linux-arm64.so \
	   shim/build/i2cshim-linux-arm.so internal/nodes/shim/
	@echo "copied all three libraries into internal/nodes/shim (the binary embeds them)"

# Replays Portduino's exact I2C call sequences against the shim and asserts the
# decoded readings. No containers, so it runs in CI; it also replays the armv7
# object under qemu-user when the host has it.
shim-test:
	./shim/test.sh

# The example plugin as an installable bundle.
plugin-example:
	./scripts/bundle-plugin.sh examples/plugins/hello ./examples/plugins/hello $(DIST)/hello-plugin.zip

clean:
	rm -rf bin $(DIST)
