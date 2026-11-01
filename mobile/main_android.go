package main

/*
#cgo LDFLAGS: -llog
#include <stdlib.h>
#include "awgjni.h"
*/
import "C"

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	_ "time/tzdata" // fallback when the system zone files cannot be read

	"awgpilot/internal/store"
)

func main() {}

var (
	appMu sync.Mutex
	app   *App
)

func goString(env *C.JNIEnv, s C.jstring) string {
	var n C.int
	p := C.awg_jstr(env, s, &n)
	if p == nil {
		return ""
	}
	defer C.awg_free(unsafe.Pointer(p))
	return fromUTF16(unsafe.Slice((*uint16)(unsafe.Pointer(p)), int(n)))
}

func fromUTF16(u []uint16) string { return string(utf16.Decode(u)) }

func toUTF16(s string) []uint16 {
	u := utf16.Encode([]rune(s))
	if len(u) == 0 {
		u = []uint16{0} // a valid pointer; the length stays 0
		return u[:0:1]
	}
	return u
}

func ptr(u []uint16) *C.uint16_t { return (*C.uint16_t)(unsafe.Pointer(unsafe.SliceData(u[:cap(u)]))) }

func javaString(env *C.JNIEnv, s string) C.jstring {
	u := toUTF16(s)
	return C.awg_newstr(env, ptr(u), C.int(len(u)))
}

// jniPlatform calls back into com.awgpilot.app.Core.
type jniPlatform struct{}

func (jniPlatform) Establish(plan Plan) (int, error) {
	b, _ := json.Marshal(plan)
	u := toUTF16(string(b))
	var n C.int
	p := C.awg_establish(ptr(u), C.int(len(u)), &n)
	if p == nil {
		return -1, errors.New("Android не создал VPN-интерфейс")
	}
	defer C.awg_free(unsafe.Pointer(p))
	res := fromUTF16(unsafe.Slice((*uint16)(unsafe.Pointer(p)), int(n)))
	if fd, ok := strings.CutPrefix(res, "fd:"); ok {
		return strconv.Atoi(fd)
	}
	return -1, errors.New(strings.TrimPrefix(res, "err:"))
}

func (jniPlatform) Release() { C.awg_release() }

func (jniPlatform) Notify(title, body string) {
	t, b := toUTF16(title), toUTF16(body)
	C.awg_notify(ptr(t), C.int(len(t)), ptr(b), C.int(len(b)))
}

func (jniPlatform) Owner(proto int, local string, lport int, remote string, rport int) string {
	l, r := toUTF16(local), toUTF16(remote)
	var n C.int
	p := C.awg_owner(C.int(proto), ptr(l), C.int(len(l)), C.int(lport), ptr(r), C.int(len(r)), C.int(rport), &n)
	if p == nil {
		return ""
	}
	defer C.awg_free(unsafe.Pointer(p))
	return fromUTF16(unsafe.Slice((*uint16)(unsafe.Pointer(p)), int(n)))
}

//export Java_com_awgpilot_app_Core_nativeInit
func Java_com_awgpilot_app_Core_nativeInit(env *C.JNIEnv, cls C.jclass, dir, keyHex, tz C.jstring, sdk C.jint) C.jstring {
	appMu.Lock()
	defer appMu.Unlock()
	if app != nil {
		return javaString(env, "")
	}
	// Android sets no TZ variable, so Go would log in UTC without this.
	if loc, err := time.LoadLocation(goString(env, tz)); err == nil {
		time.Local = loc
	}
	store.RequireKey = true
	if k, err := hex.DecodeString(goString(env, keyHex)); err == nil && len(k) == 32 {
		store.SecretKey = k
	}
	a, err := newApp(jniPlatform{}, goString(env, dir), int(sdk), func(line string) {
		cs := C.CString(line)
		C.awg_log(cs)
		C.free(unsafe.Pointer(cs))
	})
	if err != nil {
		return javaString(env, err.Error())
	}
	app = a
	return javaString(env, "")
}

//export Java_com_awgpilot_app_Core_nativeCall
func Java_com_awgpilot_app_Core_nativeCall(env *C.JNIEnv, cls C.jclass, method, args C.jstring) C.jstring {
	appMu.Lock()
	a := app
	appMu.Unlock()
	if a == nil {
		return javaString(env, reply(nil, errors.New("ядро не запущено")))
	}
	return javaString(env, a.Call(goString(env, method), goString(env, args)))
}
