package main

/*
#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <stdatomic.h>
#include <limits.h>

typedef struct { void *ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void *, const char *, const uint8_t *, size_t, cliproxy_buffer *);
typedef void (*cliproxy_host_free_fn)(void *, size_t);
typedef struct {
	uint32_t abi_version;
	void *host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char *, uint8_t *, size_t, cliproxy_buffer *);
typedef void (*cliproxy_plugin_free_fn)(void *, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char *, uint8_t *, size_t, cliproxy_buffer *);
extern void cliproxyPluginFree(void *, size_t);
extern void cliproxyPluginShutdown(void);

static cliproxy_host_api stored_host;
static _Atomic int initialized;

static int claim_plugin(const cliproxy_host_api *host, cliproxy_plugin_api *plugin) {
	if (!host || !plugin || host->abi_version != 1 || !host->call || !host->free_buffer) return 1;
	if (atomic_exchange(&initialized, 1)) return 1;
	return 0;
}

static void configure_plugin(const cliproxy_host_api *host, cliproxy_plugin_api *plugin) {
	stored_host = *host;
	plugin->abi_version = 1;
	plugin->call = cliproxyPluginCall;
	plugin->free_buffer = cliproxyPluginFree;
	plugin->shutdown = cliproxyPluginShutdown;
}

static int call_host(const char *method, const uint8_t *request, size_t request_len, cliproxy_buffer *response) {
	return stored_host.call(stored_host.host_ctx, method, request, request_len, response);
}
static void free_host_buffer(void *ptr, size_t len) { stored_host.free_buffer(ptr, len); }
static int fits_go_bytes(size_t len) { return len <= INT_MAX; }
*/
import "C"

import (
	"unsafe"
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, output *C.cliproxy_plugin_api) (status C.int) {
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if C.claim_plugin(host, output) != 0 {
		return 1
	}
	err := pluginRuntime.Initialize(&plugin{runtime: &pluginRuntime}, func(method string, request []byte) ([]byte, int) {
		cMethod := C.CString(method)
		defer C.free(unsafe.Pointer(cMethod))
		var requestPtr *C.uint8_t
		if len(request) > 0 {
			requestPtr = (*C.uint8_t)(unsafe.Pointer(&request[0]))
		}
		var response C.cliproxy_buffer
		rc := C.call_host(cMethod, requestPtr, C.size_t(len(request)), &response)
		var body []byte
		valid := (response.ptr != nil || response.len == 0) && C.fits_go_bytes(response.len) != 0
		if valid && response.ptr != nil && response.len > 0 {
			body = C.GoBytes(response.ptr, C.int(response.len))
		}
		if response.ptr != nil {
			C.free_host_buffer(response.ptr, response.len)
		}
		if !valid {
			return nil, 1
		}
		return body, int(rc)
	})
	if err != nil {
		return 1
	}
	C.configure_plugin(host, output)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (status C.int) {
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if method == nil || response == nil || (request == nil && requestLen > 0) || C.fits_go_bytes(requestLen) == 0 {
		return 1
	}
	response.ptr, response.len = nil, 0
	var body []byte
	if request != nil && requestLen > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, rc := pluginRuntime.Call(C.GoString(method), body)
	if uint64(len(result)) > uint64(^uint32(0)>>1) {
		return 1
	}
	if len(result) > 0 {
		response.ptr = C.CBytes(result)
		response.len = C.size_t(len(result))
	}
	return C.int(rc)
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) { C.free(ptr) }

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	pluginRuntime.Shutdown()
}

func main() {}
