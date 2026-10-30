package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	void *ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void *host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int goPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void goPluginFree(void*, size_t);
extern void goPluginShutdown(void);

static void configure_plugin_api(cliproxy_plugin_api *api, uint32_t version) {
	api->abi_version = version;
	api->call = (cliproxy_plugin_call_fn)goPluginCall;
	api->free_buffer = goPluginFree;
	api->shutdown = goPluginShutdown;
}

static void clear_plugin_api(cliproxy_plugin_api *api) {
	memset(api, 0, sizeof(*api));
}

static int call_host(cliproxy_host_api *api, const char *method, const uint8_t *request, size_t request_len, cliproxy_buffer *response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(cliproxy_host_api *api, void *ptr, size_t len) {
	api->free_buffer(ptr, len);
}

static size_t bounded_method_length(const char *method) {
	size_t length = strnlen(method, 257);
	return length > 256 ? SIZE_MAX : length;
}
*/
import "C"

import (
	"sync"
	"unsafe"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const maxCGoBytes = int(^uint32(0) >> 1)

var (
	runtime nativeabi.Runtime
	hostMu  sync.RWMutex
	hostAPI *C.cliproxy_host_api
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(input *C.cliproxy_host_api, output *C.cliproxy_plugin_api) C.int {
	if output == nil {
		return 1
	}
	C.clear_plugin_api(output)
	if input == nil || input.abi_version != C.uint32_t(nativeabi.ABIVersion) || input.call == nil || input.free_buffer == nil {
		return 1
	}
	if err := runtime.Initialize(newHandler(), callHost); err != nil {
		return 2
	}
	hostMu.Lock()
	hostAPI = input
	hostMu.Unlock()
	C.configure_plugin_api(output, C.uint32_t(nativeabi.ABIVersion))
	return 0
}

//export goPluginCall
func goPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	if method == nil {
		return 1
	}
	methodLen := C.bounded_method_length(method)
	if methodLen == C.SIZE_MAX || uint64(requestLen) > uint64(maxCGoBytes) || (request == nil && requestLen != 0) {
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	encoded, status := runtime.Call(C.GoStringN(method, C.int(methodLen)), payload)
	if len(encoded) > 0 {
		response.ptr = C.CBytes(encoded)
	}
	response.len = C.size_t(len(encoded))
	return C.int(status)
}

//export goPluginFree
func goPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	C.free(ptr)
}

//export goPluginShutdown
func goPluginShutdown() {
	runtime.Shutdown()
	hostMu.Lock()
	hostAPI = nil
	hostMu.Unlock()
}

func callHost(method string, request []byte) ([]byte, int) {
	hostMu.RLock()
	api := hostAPI
	hostMu.RUnlock()
	if api == nil || len(request) > maxCGoBytes {
		return nil, 1
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(request) > 0 {
		requestPtr = (*C.uint8_t)(unsafe.Pointer(&request[0]))
	}
	var response C.cliproxy_buffer
	status := C.call_host(api, cMethod, requestPtr, C.size_t(len(request)), &response)
	var encoded []byte
	if response.ptr != nil && uint64(response.len) <= uint64(maxCGoBytes) {
		encoded = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(api, response.ptr, response.len)
	}
	if (response.ptr == nil && response.len != 0) || uint64(response.len) > uint64(maxCGoBytes) {
		return nil, 1
	}
	return encoded, int(status)
}

func main() {}
