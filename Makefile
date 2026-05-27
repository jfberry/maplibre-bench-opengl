FFI_DIR ?= /Users/james/dev/maplibre-native-ffi
VARIANT ?= linux-arm64-egl
BUILD_DIR := $(FFI_DIR)/build/$(VARIANT)
INCLUDE_DIR := $(FFI_DIR)/include

export CGO_CFLAGS  := -I$(INCLUDE_DIR)
export CGO_LDFLAGS := -L$(BUILD_DIR) -Wl,-rpath,$(BUILD_DIR) -lmaplibre-native-c

DOCKER_IMG ?= maplibre-go-readback

# Fixtures for the UK walk benchmark: klokantech-basic style + GB mbtiles.
# The prepared style.json embeds absolute paths under FIXTURES_DIR, so the
# container must see the same paths the host does — bind-mount FIXTURES_DIR
# at the same location inside the container.
FIXTURES_DIR ?= /Users/james/GolandProjects/rampardos-tileserver-replacement
UK_STYLE := $(FIXTURES_DIR)/TileServer/Styles/klonkantech-basic/style.prepared.json

.PHONY: build run shell ensure-lib clean uk-walk

build: ensure-lib
	go build -o bench .

# Runs the bench inside the maplibre-go-readback container so Mesa llvmpipe is
# available. Pass extra flags via BENCH_ARGS.
#
#   make run BENCH_ARGS="--style https://demotiles.maplibre.org/style.json --frames 20"
BENCH_ARGS ?= --style https://demotiles.maplibre.org/style.json --warmup 2 --frames 10
run: ensure-lib build
	docker run --rm --platform=linux/arm64 \
	  -v $(PWD):$(PWD) \
	  -v $(FFI_DIR):$(FFI_DIR) \
	  -e EGL_PLATFORM=surfaceless \
	  -e LIBGL_ALWAYS_SOFTWARE=true \
	  -w $(PWD) \
	  $(DOCKER_IMG) \
	  ./bench $(BENCH_ARGS)

# UK walk: 1000 frames panning Scotland -> southeast across Great Britain
# against the klokantech-basic style + GB mbtiles. Mirrors the workload the
# archive's bench used to drive.
UK_WALK_ARGS ?= --frames 1000 --warmup 10 --zoom 15 --frame-timeout 30s --load-timeout 60s
uk-walk: ensure-lib build
	@test -f "$(UK_STYLE)" || (echo "UK style missing: $(UK_STYLE)" >&2; exit 1)
	docker run --rm --platform=linux/arm64 \
	  -v $(PWD):$(PWD) \
	  -v $(FFI_DIR):$(FFI_DIR) \
	  -v $(FIXTURES_DIR):$(FIXTURES_DIR):ro \
	  -e EGL_PLATFORM=surfaceless \
	  -e LIBGL_ALWAYS_SOFTWARE=true \
	  -w $(PWD) \
	  $(DOCKER_IMG) \
	  ./bench --style "$(UK_STYLE)" $(UK_WALK_ARGS)

shell: ensure-lib
	docker run --rm -it --platform=linux/arm64 \
	  -v $(PWD):$(PWD) \
	  -v $(FFI_DIR):$(FFI_DIR) \
	  -e EGL_PLATFORM=surfaceless \
	  -e LIBGL_ALWAYS_SOFTWARE=true \
	  -w $(PWD) \
	  $(DOCKER_IMG) \
	  bash

# Extract the linux-arm64-egl shared library from the docker image so the host
# can also link against it (the cgo build runs inside the container, but the
# linker invocation refers to the host-mounted path).
ensure-lib:
	@test -f $(BUILD_DIR)/libmaplibre-native-c.so || (mkdir -p $(BUILD_DIR) && docker run --rm --platform=linux/arm64 -v $(FFI_DIR)/build:/host-build $(DOCKER_IMG) cp -r /work/build/$(VARIANT)/. /host-build/$(VARIANT)/)

clean:
	rm -f bench
