package agen

/*
#cgo CFLAGS: -I${SRCDIR}/../../../engine/crates/agen-ffi/include
#cgo windows LDFLAGS: -L${SRCDIR}/../../../target/x86_64-pc-windows-gnu/release -l:libagen_ffi.a -static -lbcrypt -lkernel32 -lntdll -luserenv -lws2_32 -ldbghelp
#cgo linux LDFLAGS: -L${SRCDIR}/../../../target/release -lagen_ffi -lm -ldl -lpthread
#cgo darwin LDFLAGS: -L${SRCDIR}/../../../target/release -lagen_ffi -framework Security -framework CoreFoundation -framework SystemConfiguration
#include <stdlib.h>
#include "agen.h"

extern void goAgenHost(void *user_data, char *request_json);
extern void goAgenEvent(void *user_data, char *event_json);

static void agen_host_trampoline(void *ud, const char *req) { goAgenHost(ud, (char *)req); }
static void agen_event_trampoline(void *ud, const char *ev) { goAgenEvent(ud, (char *)ev); }

static AgenAgent *agen_new_with_host(const char *spec, uintptr_t h, char **err) {
	return agen_agent_new(spec, agen_host_trampoline, (void *)h, err);
}
static char *agen_run_with_events(const AgenAgent *a, const char *in, const char *opts, uintptr_t h, char **err) {
	return agen_run(a, in, opts, agen_event_trampoline, (void *)h, err);
}
*/
import "C"

import (
	"runtime/cgo"

	"unsafe"
)

//export goAgenHost
func goAgenHost(ud unsafe.Pointer, req *C.char) {
	// A late callback after Close (handle already deleted) is ignored; the
	// engine has already failed every pending request at shutdown.
	defer func() { _ = recover() }()
	a := cgo.Handle(uintptr(ud)).Value().(*Agent)
	a.onHostRequest(C.GoString(req))
}

//export goAgenEvent
func goAgenEvent(ud unsafe.Pointer, ev *C.char) {
	defer func() { _ = recover() }()
	r := cgo.Handle(uintptr(ud)).Value().(*runState)
	r.onEvent(C.GoString(ev))
}

func takeString(p *C.char) string {
	if p == nil {
		return ""
	}
	s := C.GoString(p)
	C.agen_string_free(p)
	return s
}

func ffiNew(spec string, h cgo.Handle) (*C.AgenAgent, string) {
	cs := C.CString(spec)
	defer C.free(unsafe.Pointer(cs))
	var errOut *C.char
	ptr := C.agen_new_with_host(cs, C.uintptr_t(h), &errOut)
	return ptr, takeString(errOut)
}

func ffiRun(a *C.AgenAgent, input, opts string, h cgo.Handle) (string, string) {
	ci, co := C.CString(input), C.CString(opts)
	defer C.free(unsafe.Pointer(ci))
	defer C.free(unsafe.Pointer(co))
	var errOut *C.char
	res := C.agen_run_with_events(a, ci, co, C.uintptr_t(h), &errOut)
	return takeString(res), takeString(errOut)
}

func ffiComplete(a *C.AgenAgent, id, result string, isErr bool) bool {
	cid, cr := C.CString(id), C.CString(result)
	defer C.free(unsafe.Pointer(cid))
	defer C.free(unsafe.Pointer(cr))
	e := C.int32_t(0)
	if isErr {
		e = 1
	}
	return C.agen_complete(a, cid, cr, e) == 1
}

func ffiCancel(a *C.AgenAgent, key string) bool {
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	return C.agen_cancel(a, ck) == 1
}

func ffiTrace(a *C.AgenAgent, id string) (string, string) {
	cid := C.CString(id)
	defer C.free(unsafe.Pointer(cid))
	var errOut *C.char
	res := C.agen_trace(a, cid, &errOut)
	return takeString(res), takeString(errOut)
}

func ffiShutdown(a *C.AgenAgent) { C.agen_shutdown(a) }

func ffiFree(a *C.AgenAgent) { C.agen_agent_free(a) }

// Version of the linked engine library.
func Version() string { return C.GoString(C.agen_version()) }
