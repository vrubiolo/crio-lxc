GO_SRC=$(shell find . -name \*.go)
COMMIT_HASH=$(shell git describe --always --tags --long)
COMMIT=$(if $(shell git status --porcelain --untracked-files=no),$(COMMIT_HASH)-dirty,$(COMMIT_HASH))
TEST?=$(patsubst test/%.bats,%,$(wildcard test/*.bats))
PACKAGES_DIR?=~/packages
BINS := crio-lxc crio-lxc-start crio-lxc-init crio-lxc-hook
PREFIX ?= /usr/local
PKG_CONFIG_PATH ?= $(PREFIX)/lib/pkgconfig
export PKG_CONFIG_PATH
LDFLAGS=-X main.version=$(COMMIT)

all: fmt $(BINS)

install: all
	cp $(BINS) $(PREFIX)/bin

lint:
	golangci-lint run -c ./lint.yaml ./...

crio-lxc: $(GO_SRC) Makefile go.mod
	go build -ldflags '$(LDFLAGS)' -o $@ ./cmd

crio-lxc-start: cmd/start/crio-lxc-start.c
	cc -Wall $(shell PKG_CONFIG_PATH=$(PKG_CONFIG_PATH) pkg-config --libs --cflags lxc) $? -o $@

crio-lxc-init: $(GO_SRC) Makefile go.mod
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS) -extldflags "-static"' -o $@ ./cmd/init
	# ensure that crio-lxc-init is statically compiled
	! ldd $@  2>/dev/null

crio-lxc-hook: $(GO_SRC) Makefile go.mod
	go build -ldflags '$(LDFLAGS) -extldflags "-static"' -o $@ ./cmd/hook

# make test TEST=basic will run only the basic test.
.PHONY: check
check: crio-lxc
	go fmt ./... && ([ -z $(TRAVIS) ] || git diff --quiet)
	go test ./...
	PACKAGES_DIR=$(PACKAGES_DIR) sudo -E "PATH=$$PATH" bats -t $(patsubst %,test/%.bats,$(TEST))

.PHONY: vendorup
vendorup:
	go get -u

.PHONY: clean
clean:
	-rm -f $(BINS)

fmt:
	go fmt ./...
