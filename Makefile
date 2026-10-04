# Shortcuts to scripts/ - the scripts are the source of truth (see RELEASING.md).

.PHONY: all build test dist image release-dry-run clean

all: test build

build:
	scripts/build.sh

test:
	scripts/test.sh --race

dist:
	scripts/dist.sh

image:
	scripts/image.sh

# what the next patch release would do, without changing or publishing anything
release-dry-run:
	scripts/release.sh patch --dry-run --allow-dirty

clean:
	rm -rf bin dist coverage.out
