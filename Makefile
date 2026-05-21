.PHONY: build install test vet clean

BINARY      := tsserve
BINDIR      := /usr/local/bin
UNITDIR     := /etc/systemd/system
CONFDIR     := /etc/tsserve

GIT_SHA     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS     := -w -s -X main.commit=$(GIT_SHA)

build: $(BINARY)

$(BINARY):
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o $(BINARY) .

# Install tsserve as a systemd service to /. Run as root.
# Intended to be invoked from an image-build pipeline (Packer, Ansible, ...)
# that has checked this repo out into a temp directory on the target host.
# Idempotent; safe to re-run.
install: $(BINARY)
	id tsserve >/dev/null 2>&1 || useradd --system --no-create-home --shell /sbin/nologin tsserve
	install -d -m 0755 $(BINDIR)
	install -o root -g root -m 0755 $(BINARY) $(BINDIR)/$(BINARY)
	install -d -o root -g tsserve -m 0750 $(CONFDIR)
	install -o root -g tsserve -m 0640 systemd/tsserve.env.example $(CONFDIR)/tsserve.env.example
	install -o root -g root -m 0644 systemd/tsserve.service $(UNITDIR)/tsserve.service
	systemctl daemon-reload
	systemctl enable tsserve.service

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
