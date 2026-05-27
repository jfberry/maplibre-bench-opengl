# maplibre-bench-opengl

Single-frame bench harness for the upstream MapLibre Native Go binding's
OpenGL EGL render target. Ported from `cmd/bench` in the
`jfberry/maplibre-native-go` archive, with three changes:

1. Imports `github.com/maplibre/maplibre-native-ffi/bindings/go` (the upstream
   binding) via a `replace` directive pointing at the local checkout.
2. Linux-only — drives a surfaceless EGL context and attaches an
   `OpenGLOwnedTexture` session.
3. Implements `RenderStill` as an explicit `RunOnce`/`PollEvent`/`RenderUpdate`
   pump rather than relying on a binding-provided helper.

The measured workload mirrors the archive's bench: warmup frames → measured
frames with a camera pan across a grid → p50/p90/p99/max frame time, FPS,
max-RSS progression, Go heap.

## Build & run via docker

`make run` builds the linux/arm64 native lib (extracting it from the
`maplibre-go-readback` docker image we built earlier — see
`examples/go-readback/Dockerfile` in `maplibre-native-ffi`), compiles the
bench binary against it, then runs the binary inside the same container so
Mesa llvmpipe is available.

```
make run BENCH_ARGS="--style https://demotiles.maplibre.org/style.json --frames 50"
```

For a quick smoke check with the default args (small frame count, MapLibre
demo style):

```
make run
```

## Environment

- `FFI_DIR` (default `/Users/james/dev/maplibre-native-ffi`) — upstream repo
  with the linux-arm64-egl native build available.
- `VARIANT` (default `linux-arm64-egl`).
- `DOCKER_IMG` (default `maplibre-go-readback`) — the image with mise + pixi +
  Mesa software EGL already configured.

`make ensure-lib` extracts `libmaplibre-native-c.so` from the docker image
into `$FFI_DIR/build/$VARIANT/` so the host-side cgo link step can find it.

## CLI flags

Same set as the archive's bench:

```
--style       style URL, file path, or inline JSON (required)
--lat, --lon  starting camera center
--zoom, --bearing, --pitch
--w, --h, --scale
--warmup      warmup frames before measurement (default 5)
--frames      measured frames (default 100)
--dlat, --dlon  camera delta applied per measured frame
--frame-timeout  per-frame render-still timeout (default 10s)
--load-timeout   style-load timeout (default 15s)
-v            log per-frame timings
```
