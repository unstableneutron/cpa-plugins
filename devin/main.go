package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <limits.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static const cliproxy_host_api* stored_host;
static int valid_host_api(const cliproxy_host_api* host) { return host != NULL && host->call != NULL && host->free_buffer != NULL; }
static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }
static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
  if (!valid_host_api(stored_host)) return 1;
  return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}
static void free_host_buffer(void* ptr, size_t len) {
  if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) stored_host->free_buffer(ptr, len);
}
*/
import "C"

import (
	"unsafe"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

var runtime nativeabi.Runtime

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil || C.valid_host_api(host) == 0 || uint32(host.abi_version) != nativeabi.ABIVersion {
		return 1
	}
	if err := runtime.Initialize(provider{}, hostCall); err != nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(nativeabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	if method == nil || (request == nil && requestLen != 0) || uint64(requestLen) > uint64(^uint32(0)>>1) {
		return 1
	}
	var requestBytes []byte
	if requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, status := runtime.Call(C.GoString(method), requestBytes)
	writeResponse(response, raw)
	return C.int(status)
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) { C.free(ptr) }

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { runtime.Shutdown() }

func hostCall(method string, request []byte) ([]byte, int) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cRequest unsafe.Pointer
	if len(request) > 0 {
		cRequest = C.CBytes(request)
		defer C.free(cRequest)
	}
	var response C.cliproxy_buffer
	status := C.call_host_api(cMethod, (*C.uint8_t)(cRequest), C.size_t(len(request)), &response)
	if response.ptr != nil {
		defer C.free_host_buffer(response.ptr, response.len)
	}
	if response.len == 0 {
		return []byte{}, int(status)
	}
	if response.ptr == nil || uint64(response.len) > uint64(^uint32(0)>>1) {
		return nil, 1
	}
	return C.GoBytes(response.ptr, C.int(response.len)), int(status)
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if len(raw) == 0 {
		response.ptr = C.malloc(1)
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}
