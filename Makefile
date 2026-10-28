PLUGIN_ID := flux
# The plugin is UI-only, so every platform gets the same archive.
PLATFORMS ?= darwin_arm64 darwin_amd64 linux_amd64 linux_arm64 windows_amd64
# Releases pass the tag here; the default is whatever plugin.yaml says.
VERSION ?= $(shell sed -n 's/^version: *//p' plugin.yaml)

.PHONY: init build package clean

init:
	cd ui && pnpm install --frozen-lockfile

build: init
	cd ui && pnpm build

# Produces dist/<id>-<os>_<arch>.tar.gz, the names publish-plugin-action uploads.
# Layout is what the app installs: plugin.yaml and assets/ at the archive root.
# The empty bin/ is only there because the marketplace validator requires one.
package: build
	rm -rf dist
	mkdir -p dist/pkg/assets dist/pkg/bin
	sed "s/^version: .*/version: $(VERSION)/" plugin.yaml > dist/pkg/plugin.yaml
	cp -R ui/dist/assets/. dist/pkg/assets/
	for platform in $(PLATFORMS); do \
		COPYFILE_DISABLE=1 tar -czf dist/$(PLUGIN_ID)-$$platform.tar.gz -C dist/pkg plugin.yaml assets bin; \
	done
	rm -rf dist/pkg

clean:
	rm -rf dist ui/dist
