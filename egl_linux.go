//go:build linux

package main

/*
#cgo linux pkg-config: egl

#include <EGL/egl.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>

typedef struct mln_bench_egl {
    EGLDisplay display;
    EGLConfig  config;
    EGLContext share_context;
    EGLSurface surface;
} mln_bench_egl;

static int mln_bench_egl_init(mln_bench_egl *out, char *err, size_t err_len) {
    memset(out, 0, sizeof(*out));

    out->display = eglGetDisplay(EGL_DEFAULT_DISPLAY);
    if (out->display == EGL_NO_DISPLAY) {
        snprintf(err, err_len, "eglGetDisplay returned EGL_NO_DISPLAY (0x%x)", eglGetError());
        return -1;
    }
    EGLint major = 0, minor = 0;
    if (eglInitialize(out->display, &major, &minor) == EGL_FALSE) {
        snprintf(err, err_len, "eglInitialize failed (0x%x)", eglGetError());
        return -2;
    }
    if (eglBindAPI(EGL_OPENGL_ES_API) == EGL_FALSE) {
        snprintf(err, err_len, "eglBindAPI(EGL_OPENGL_ES_API) failed (0x%x)", eglGetError());
        return -3;
    }

    EGLint config_attribs[] = {
        EGL_SURFACE_TYPE,    EGL_PBUFFER_BIT,
        EGL_RENDERABLE_TYPE, EGL_OPENGL_ES3_BIT,
        EGL_RED_SIZE,        8,
        EGL_GREEN_SIZE,      8,
        EGL_BLUE_SIZE,       8,
        EGL_ALPHA_SIZE,      8,
        EGL_DEPTH_SIZE,      24,
        EGL_STENCIL_SIZE,    8,
        EGL_NONE
    };
    EGLint config_count = 0;
    if (eglChooseConfig(out->display, config_attribs, &out->config, 1, &config_count) == EGL_FALSE ||
        config_count == 0 || out->config == NULL) {
        snprintf(err, err_len, "eglChooseConfig found no config (0x%x)", eglGetError());
        return -4;
    }

    EGLint context_attribs[] = {
        EGL_CONTEXT_CLIENT_VERSION, 3,
        EGL_NONE
    };
    out->share_context = eglCreateContext(out->display, out->config, EGL_NO_CONTEXT, context_attribs);
    if (out->share_context == EGL_NO_CONTEXT) {
        snprintf(err, err_len, "eglCreateContext failed (0x%x)", eglGetError());
        return -5;
    }

    EGLint surface_attribs[] = {
        EGL_WIDTH,  8,
        EGL_HEIGHT, 8,
        EGL_NONE
    };
    out->surface = eglCreatePbufferSurface(out->display, out->config, surface_attribs);
    if (out->surface == EGL_NO_SURFACE) {
        snprintf(err, err_len, "eglCreatePbufferSurface failed (0x%x)", eglGetError());
        return -6;
    }

    if (eglMakeCurrent(out->display, out->surface, out->surface, out->share_context) == EGL_FALSE) {
        snprintf(err, err_len, "eglMakeCurrent failed (0x%x)", eglGetError());
        return -7;
    }
    return 0;
}

static void mln_bench_egl_destroy(mln_bench_egl *ctx) {
    if (ctx == NULL || ctx->display == EGL_NO_DISPLAY) return;
    eglMakeCurrent(ctx->display, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
    if (ctx->surface != EGL_NO_SURFACE) eglDestroySurface(ctx->display, ctx->surface);
    if (ctx->share_context != EGL_NO_CONTEXT) eglDestroyContext(ctx->display, ctx->share_context);
    eglTerminate(ctx->display);
    memset(ctx, 0, sizeof(*ctx));
}

static void *mln_bench_egl_get_proc_address(void) {
    return (void *)eglGetProcAddress;
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

// eglContext owns the EGL handles backing one bench run.
type eglContext struct {
	raw C.mln_bench_egl
}

func newEGLContext() (*eglContext, error) {
	c := &eglContext{}
	var errBuf [256]C.char
	if rc := C.mln_bench_egl_init(&c.raw, &errBuf[0], C.size_t(len(errBuf))); rc != 0 {
		return nil, fmt.Errorf("EGL init: %s", C.GoString(&errBuf[0]))
	}
	return c, nil
}

func (c *eglContext) descriptor() maplibre.OpenGLContextDescriptor {
	return maplibre.NewOpenGLContextEGL(maplibre.EglContextDescriptor{
		Display:        maplibre.NativePointer(uintptr(unsafe.Pointer(c.raw.display))),
		Config:         maplibre.NativePointer(uintptr(unsafe.Pointer(c.raw.config))),
		ShareContext:   maplibre.NativePointer(uintptr(unsafe.Pointer(c.raw.share_context))),
		GetProcAddress: maplibre.NativePointer(uintptr(C.mln_bench_egl_get_proc_address())),
	})
}

func (c *eglContext) close() {
	C.mln_bench_egl_destroy(&c.raw)
}

func attachSession(m *maplibre.MapHandle, egl *eglContext, width, height uint32, scaleFactor float64, target string) (*maplibre.RenderSessionHandle, error) {
	extent := maplibre.RenderTargetExtent{Width: width, Height: height, ScaleFactor: scaleFactor}
	switch target {
	case "offscreen":
		return m.AttachOpenGLOffscreen(maplibre.OpenGLOffscreenDescriptor{
			Extent:  extent,
			Context: egl.descriptor(),
		})
	default:
		return m.AttachOpenGLOwnedTexture(maplibre.OpenGLOwnedTextureDescriptor{
			Extent:  extent,
			Context: egl.descriptor(),
		})
	}
}
