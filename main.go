//go:build linux

// Command maplibre-bench-opengl renders many frames against a real style to
// measure per-frame timing and steady-state RSS through the upstream MapLibre
// Native Go binding's OpenGL EGL path.
//
// Ported from cmd/bench in the jfberry/maplibre-native-go archive. Differences
// from the original:
//
//   - Uses the upstream binding (github.com/maplibre/maplibre-native-ffi/
//     bindings/go) so it exercises the OpenGL render target API added in
//     bindings/go/render.go.
//   - Linux-only — drives EGL surfaceless context creation through the
//     attached helper, then attaches an OpenGLOwnedTexture session.
//   - Replaces the archive's RenderStill convenience with an explicit
//     RunOnce/PollEvent/RenderUpdate pump, matching the pattern upstream's
//     binding exposes.
//   - Acquires + closes an OpenGLOwnedTextureFrame each iteration to mirror
//     the archive's Acquire/ReleaseFrame timing surface (no CPU readback).
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

// Great Britain mainland bounding box. Wide enough to defeat tile-cache
// reuse across consecutive random samples (each ~110×80 km step would land
// in a completely different region), narrow enough to stay within the GB
// mbtiles coverage so every random viewport hits real tile data.
const (
	gbLatMin = 50.0
	gbLatMax = 58.5
	gbLonMin = -6.0
	gbLonMax = 1.5
)

const renderBudgetDefault = 10 * time.Second

func main() {
	style := flag.String("style", "", "style URL, file path, or inline JSON (required)")
	lat := flag.Float64("lat", 55.07, "starting latitude")
	lon := flag.Float64("lon", -3.58, "starting longitude")
	zoom := flag.Float64("zoom", 8, "starting zoom")
	bearing := flag.Float64("bearing", 0, "starting bearing")
	pitch := flag.Float64("pitch", 0, "starting pitch")
	width := flag.Uint("w", 512, "logical width")
	height := flag.Uint("h", 512, "logical height")
	scale := flag.Float64("scale", 1, "scale factor")
	warmup := flag.Int("warmup", 5, "warmup frames before measurement")
	frames := flag.Int("frames", 100, "frames to render after warmup")
	dLat := flag.Float64("dlat", 0.01, "lat delta per measured frame")
	dLon := flag.Float64("dlon", 0.01, "lon delta per measured frame")
	frameTimeout := flag.Duration("frame-timeout", renderBudgetDefault, "per-frame render-still timeout")
	loadTimeout := flag.Duration("load-timeout", 15*time.Second, "style-load timeout")
	verbose := flag.Bool("v", false, "log per-frame timings")
	target := flag.String("target", "owned-texture", "render target: owned-texture | offscreen")
	walkMode := flag.String("walk-mode", "linear", "linear (current default — adjacent steps) | random-gb (uniformly random viewports in the GB bbox; defeats tile cache)")
	walkSeed := flag.Uint64("walk-seed", 0xC0FFEE, "PRNG seed for --walk-mode=random-gb (reproducibility)")
	flag.Parse()

	if *style == "" {
		log.Fatalf("--style is required")
	}

	// The upstream binding does not own a dispatcher goroutine; every map and
	// runtime call must originate from the OS thread that created the runtime.
	// Pin this goroutine here so all subsequent native calls land on the same
	// thread, which is also the EGL-current thread.
	runtime.LockOSThread()

	log.SetFlags(log.Lmicroseconds)
	log.Printf("ABI v%d", maplibre.CVersion())

	backends := maplibre.SupportedRenderBackends()
	if !backends.Has(maplibre.RenderBackendOpenGL) {
		log.Fatalf("OpenGL backend not present in linked native library (mask=0x%x)", uint32(backends))
	}
	providers := maplibre.SupportedOpenGLContextProviders()
	if !providers.Has(maplibre.OpenGLContextProviderEGL) {
		log.Fatalf("EGL context provider not present (mask=0x%x)", uint32(providers))
	}

	egl, err := newEGLContext()
	if err != nil {
		log.Fatalf("EGL: %v", err)
	}
	defer egl.close()

	rt, err := maplibre.NewRuntime()
	if err != nil {
		log.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close()

	m, err := rt.NewMapWithOptions(maplibre.MapOptions{
		Width:       uint32(*width),
		Height:      uint32(*height),
		ScaleFactor: *scale,
		Mode:        maplibre.MapModeStatic,
	})
	if err != nil {
		log.Fatalf("NewMap: %v", err)
	}
	defer m.Close()

	if err := loadStyle(m, *style); err != nil {
		log.Fatalf("load style: %v", err)
	}
	if err := waitForStyleLoaded(rt, *loadTimeout); err != nil {
		log.Fatalf("waiting for STYLE_LOADED: %v", err)
	}
	log.Printf("style loaded")

	log.Printf("attach target: %s", *target)
	sess, err := attachSession(m, egl, uint32(*width), uint32(*height), *scale, *target)
	if err != nil {
		log.Fatalf("attachSession: %v", err)
	}
	defer sess.Close()

	// Sized to the physical pixel count so the buffer is reusable across the
	// whole bench without per-frame allocation. The bench keeps a single
	// viewport size; ReadPremultipliedRGBA8Into fills exactly what the
	// session reports back.
	physWidth := uint32(float64(*width) * *scale)
	physHeight := uint32(float64(*height) * *scale)
	readbackBuf := make([]byte, int(physWidth)*int(physHeight)*4)

	prng := rand.New(rand.NewPCG(*walkSeed, *walkSeed^0xDEADBEEF))
	nextCamera := func(i int) maplibre.LatLng {
		switch *walkMode {
		case "random-gb":
			return maplibre.LatLng{
				Latitude:  gbLatMin + prng.Float64()*(gbLatMax-gbLatMin),
				Longitude: gbLonMin + prng.Float64()*(gbLonMax-gbLonMin),
			}
		default:
			return maplibre.LatLng{
				Latitude:  *lat + float64(i)*(*dLat),
				Longitude: *lon + float64(i)*(*dLon),
			}
		}
	}
	log.Printf("walk-mode=%s (seed=%#x)", *walkMode, *walkSeed)

	jump := func(i int) error {
		camera := maplibre.CameraOptions{}.
			WithCenter(nextCamera(i)).
			WithZoom(*zoom).
			WithBearing(*bearing).
			WithPitch(*pitch)
		return m.JumpTo(camera)
	}

	var (
		frameTiles tileCounts
		measured   tileCounts
	)
	renderOne := func(i int, accumulate *tileCounts) (time.Duration, error) {
		if err := jump(i); err != nil {
			return 0, fmt.Errorf("JumpTo: %w", err)
		}
		frameTiles.reset()
		t0 := time.Now()
		if err := renderStill(rt, m, sess, *frameTimeout, &frameTiles); err != nil {
			return time.Since(t0), err
		}
		// Both targets pay for the full CPU readback path to mirror the
		// production rampardos tile-rendering workflow. The two paths thus
		// differ only in the FBO color attachment (renderbuffer vs texture)
		// and ContextMode plumbing — exactly what we want to A/B.
		if _, err := sess.ReadPremultipliedRGBA8Into(readbackBuf); err != nil {
			return time.Since(t0), fmt.Errorf("ReadPremultipliedRGBA8Into: %w", err)
		}
		dur := time.Since(t0)
		if accumulate != nil {
			accumulate.add(frameTiles)
		}
		return dur, nil
	}

	log.Printf("warmup: %d frames", *warmup)
	for i := 0; i < *warmup; i++ {
		if _, err := renderOne(i, nil); err != nil {
			log.Fatalf("warmup frame %d: %v", i, err)
		}
	}
	rssWarmup := maxRSSKB()
	log.Printf("warmup done; max-rss=%s", fmtKB(rssWarmup))

	timings := make([]time.Duration, 0, *frames)
	rssSamples := []int64{rssWarmup}

	start := time.Now()
	for i := 0; i < *frames; i++ {
		dur, err := renderOne(*warmup+i, &measured)
		if err != nil {
			log.Fatalf("frame %d: %v", i, err)
		}
		timings = append(timings, dur)
		if *verbose {
			log.Printf("frame %d: %s", i, dur)
		}
		if (i+1)%maxInt(*frames/10, 1) == 0 {
			rssSamples = append(rssSamples, maxRSSKB())
		}
	}
	elapsed := time.Since(start)
	rssEnd := maxRSSKB()

	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	p := func(q float64) time.Duration {
		idx := int(float64(len(timings)-1) * q)
		return timings[idx]
	}

	log.Printf("=== bench results ===")
	log.Printf("frames           = %d", *frames)
	log.Printf("total            = %s", elapsed.Round(time.Millisecond))
	log.Printf("fps              = %.1f", float64(*frames)/elapsed.Seconds())
	log.Printf("frame p50        = %s", p(0.5).Round(time.Microsecond))
	log.Printf("frame p90        = %s", p(0.9).Round(time.Microsecond))
	log.Printf("frame p99        = %s", p(0.99).Round(time.Microsecond))
	log.Printf("frame max        = %s", timings[len(timings)-1].Round(time.Microsecond))
	log.Printf("max-rss warmup   = %s", fmtKB(rssWarmup))
	log.Printf("max-rss end      = %s", fmtKB(rssEnd))
	log.Printf("max-rss delta    = %s", fmtKBDelta(rssEnd-rssWarmup))
	log.Printf("max-rss progression: %v", samplesProgression(rssSamples))

	var goMS runtime.MemStats
	runtime.ReadMemStats(&goMS)
	log.Printf("go heap          = %s (sys=%s)", fmtKB(int64(goMS.HeapInuse)/1024), fmtKB(int64(goMS.Sys)/1024))

	// Tile-action telemetry over the measured frames. Per-frame averages tell
	// us how many tiles each render burst actually requested, how many came
	// from cache vs network, and how much parsing happened. Useful to know
	// whether a workload is exercising the resource pipeline or just hitting
	// the in-memory tile cache.
	framesF := float64(*frames)
	log.Printf("tile/frame req   = %.1f (cache %.1f + net %.1f)",
		float64(measured.total())/framesF,
		float64(measured.requestedFromCache)/framesF,
		float64(measured.requestedFromNetwork)/framesF)
	log.Printf("tile/frame load  = %.1f (cache %.1f + net %.1f)",
		float64(measured.loadFromCache+measured.loadFromNetwork)/framesF,
		float64(measured.loadFromCache)/framesF,
		float64(measured.loadFromNetwork)/framesF)
	log.Printf("tile/frame parse = %.1f (started) %.1f (finished)",
		float64(measured.startParse)/framesF,
		float64(measured.endParse)/framesF)
	if measured.errors+measured.cancelled > 0 {
		log.Printf("tile errors/frame = %.2f, cancelled/frame = %.2f",
			float64(measured.errors)/framesF,
			float64(measured.cancelled)/framesF)
	}
}

func loadStyle(m *maplibre.MapHandle, style string) error {
	trimmed := strings.TrimLeft(style, " \t\r\n")
	switch {
	case strings.HasPrefix(trimmed, "{"):
		return m.SetStyleJSON(style)
	case strings.Contains(style, "://"):
		return m.SetStyleURL(style)
	default:
		abs := style
		if !strings.HasPrefix(abs, "/") {
			cwd, _ := os.Getwd()
			abs = cwd + "/" + abs
		}
		return m.SetStyleURL("file://" + abs)
	}
}

func waitForStyleLoaded(rt *maplibre.RuntimeHandle, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if err := rt.RunOnce(); err != nil {
			return fmt.Errorf("RunOnce: %w", err)
		}
		productive := false
		for {
			event, err := rt.PollEvent()
			if err != nil {
				return fmt.Errorf("PollEvent: %w", err)
			}
			if event == nil {
				break
			}
			productive = true
			switch event.Type {
			case maplibre.RuntimeEventMapStyleLoaded:
				return nil
			case maplibre.RuntimeEventMapLoadingFailed:
				return fmt.Errorf("map loading failed: %s", event.Message)
			}
		}
		if !productive {
			if _, err := rt.WaitForEvent(10 * time.Millisecond); err != nil {
				return fmt.Errorf("WaitForEvent: %w", err)
			}
		}
	}
	return fmt.Errorf("style load timed out after %s", budget)
}

// tileCounts tallies TileOperation events observed during a single render.
// Pre-allocated; the bench reuses one instance across frames and Reset()s
// between renders.
type tileCounts struct {
	requestedFromCache   int
	requestedFromNetwork int
	loadFromCache        int
	loadFromNetwork      int
	startParse           int
	endParse             int
	errors               int
	cancelled            int
}

func (t *tileCounts) reset() { *t = tileCounts{} }

func (t *tileCounts) total() int {
	return t.requestedFromCache + t.requestedFromNetwork
}

func (t *tileCounts) accumulate(op maplibre.TileOperation) {
	switch op {
	case maplibre.TileOperationRequestedFromCache:
		t.requestedFromCache++
	case maplibre.TileOperationRequestedFromNetwork:
		t.requestedFromNetwork++
	case maplibre.TileOperationLoadFromCache:
		t.loadFromCache++
	case maplibre.TileOperationLoadFromNetwork:
		t.loadFromNetwork++
	case maplibre.TileOperationStartParse:
		t.startParse++
	case maplibre.TileOperationEndParse:
		t.endParse++
	case maplibre.TileOperationError:
		t.errors++
	case maplibre.TileOperationCancelled:
		t.cancelled++
	}
}

func (t *tileCounts) add(other tileCounts) {
	t.requestedFromCache += other.requestedFromCache
	t.requestedFromNetwork += other.requestedFromNetwork
	t.loadFromCache += other.loadFromCache
	t.loadFromNetwork += other.loadFromNetwork
	t.startParse += other.startParse
	t.endParse += other.endParse
	t.errors += other.errors
	t.cancelled += other.cancelled
}

func renderStill(rt *maplibre.RuntimeHandle, m *maplibre.MapHandle, sess *maplibre.RenderSessionHandle, budget time.Duration, tiles *tileCounts) error {
	if err := m.RequestStillImage(); err != nil {
		return fmt.Errorf("RequestStillImage: %w", err)
	}
	rendered := false
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if err := rt.RunOnce(); err != nil {
			return fmt.Errorf("RunOnce: %w", err)
		}
		productive := false
		for {
			event, err := rt.PollEvent()
			if err != nil {
				return fmt.Errorf("PollEvent: %w", err)
			}
			if event == nil {
				break
			}
			productive = true
			switch event.Type {
			case maplibre.RuntimeEventMapTileAction:
				if tiles != nil {
					if payload, ok := event.Payload.(maplibre.RuntimeEventTileActionPayload); ok {
						tiles.accumulate(payload.Operation)
					}
				}
			case maplibre.RuntimeEventMapRenderUpdateAvailable:
				if rerr := sess.RenderUpdate(); rerr != nil {
					return fmt.Errorf("RenderUpdate: %w", rerr)
				}
				rendered = true
			case maplibre.RuntimeEventMapStillImageFinished:
				if !rendered {
					return errors.New("still image finished without a render frame")
				}
				return nil
			case maplibre.RuntimeEventMapLoadingFailed:
				return fmt.Errorf("map loading failed: %s", event.Message)
			case maplibre.RuntimeEventMapRenderError:
				return fmt.Errorf("map render error: %s", event.Message)
			case maplibre.RuntimeEventMapStillImageFailed:
				return fmt.Errorf("still image failed: %s", event.Message)
			}
		}
		if !productive {
			// Wait for a runtime event to land in the queue (or up to 10 ms).
			// WaitForEvent filters libuv-internal wakes that don't produce a
			// PollEvent-drainable event, so we don't pay a cgo crossing per
			// spurious wake.
			if _, err := rt.WaitForEvent(10 * time.Millisecond); err != nil {
				return fmt.Errorf("WaitForEvent: %w", err)
			}
		}
	}
	return fmt.Errorf("render still timed out after %s", budget)
}

func maxRSSKB() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) / 1024
	}
	return int64(ru.Maxrss)
}

func fmtKB(kb int64) string {
	if kb >= 1024 {
		return fmt.Sprintf("%.1f MiB", float64(kb)/1024)
	}
	return fmt.Sprintf("%d KiB", kb)
}

func fmtKBDelta(kb int64) string {
	sign := "+"
	if kb < 0 {
		sign = ""
	}
	return sign + fmtKB(kb)
}

func samplesProgression(samples []int64) string {
	parts := make([]string, len(samples))
	for i, s := range samples {
		parts[i] = fmtKB(s)
	}
	return strings.Join(parts, " -> ")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
