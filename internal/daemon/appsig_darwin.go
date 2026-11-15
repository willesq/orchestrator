//go:build darwin && cgo

package daemon

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

// orch_sig is what one signature check learned.
typedef struct {
	int status;           // OSStatus of the first call that failed, 0 when all passed
	uint32_t flags;       // code directory flags
	unsigned char cdhash[64];
	int cdhash_len;
	char path[4096];      // the code's path, "" when unknown
} orch_sig;

static void orch_info(SecStaticCodeRef code, SecCSFlags flags, orch_sig *out) {
	CFDictionaryRef info = NULL;
	OSStatus st = SecCodeCopySigningInformation(code, flags, &info);
	if (st != errSecSuccess) {
		out->status = st;
		return;
	}
	CFNumberRef f = CFDictionaryGetValue(info, kSecCodeInfoFlags);
	if (f != NULL && CFGetTypeID(f) == CFNumberGetTypeID()) {
		CFNumberGetValue(f, kCFNumberSInt32Type, &out->flags);
	}
	CFDataRef unique = CFDictionaryGetValue(info, kSecCodeInfoUnique);
	if (unique != NULL && CFGetTypeID(unique) == CFDataGetTypeID()) {
		CFIndex n = CFDataGetLength(unique);
		if (n > (CFIndex)sizeof(out->cdhash)) {
			n = sizeof(out->cdhash);
		}
		CFDataGetBytes(unique, CFRangeMake(0, n), out->cdhash);
		out->cdhash_len = (int)n;
	}
	CFRelease(info);
}

static SecRequirementRef orch_requirement(const char *req, orch_sig *out) {
	CFStringRef s = CFStringCreateWithCString(NULL, req, kCFStringEncodingUTF8);
	if (s == NULL) {
		out->status = errSecParam;
		return NULL;
	}
	SecRequirementRef r = NULL;
	OSStatus st = SecRequirementCreateWithString(s, kSecCSDefaultFlags, &r);
	CFRelease(s);
	if (st != errSecSuccess) {
		out->status = st;
		return NULL;
	}
	return r;
}

// orch_check_running checks the process the audit token names against req
// and reads its signature and path.
static void orch_check_running(const unsigned char *token, int token_len, const char *req, orch_sig *out) {
	memset(out, 0, sizeof(*out));
	SecRequirementRef r = orch_requirement(req, out);
	if (r == NULL) {
		return;
	}
	CFDataRef tok = CFDataCreate(NULL, token, token_len);
	if (tok == NULL) {
		out->status = errSecAllocate;
		CFRelease(r);
		return;
	}
	const void *keys[] = {kSecGuestAttributeAudit};
	const void *vals[] = {tok};
	CFDictionaryRef attrs = CFDictionaryCreate(NULL, keys, vals, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFRelease(tok);
	if (attrs == NULL) {
		out->status = errSecAllocate;
		CFRelease(r);
		return;
	}
	SecCodeRef code = NULL;
	OSStatus st = SecCodeCopyGuestWithAttributes(NULL, attrs, kSecCSDefaultFlags, &code);
	CFRelease(attrs);
	if (st != errSecSuccess) {
		out->status = st;
		CFRelease(r);
		return;
	}
	st = SecCodeCheckValidity(code, kSecCSDefaultFlags, r);
	CFRelease(r);
	if (st != errSecSuccess) {
		out->status = st;
		CFRelease(code);
		return;
	}
	CFURLRef url = NULL;
	if (SecCodeCopyPath((SecStaticCodeRef)code, kSecCSDefaultFlags, &url) == errSecSuccess) {
		if (!CFURLGetFileSystemRepresentation(url, true, (UInt8 *)out->path, sizeof(out->path))) {
			out->path[0] = 0;
		}
		out->path[sizeof(out->path) - 1] = 0;
		CFRelease(url);
	}
	orch_info((SecStaticCodeRef)code, kSecCSSigningInformation | kSecCSDynamicInformation, out);
	CFRelease(code);
}

// orch_check_static checks the code on disk at path against req and reads
// its signature.
static void orch_check_static(const char *path, const char *req, orch_sig *out) {
	memset(out, 0, sizeof(*out));
	SecRequirementRef r = orch_requirement(req, out);
	if (r == NULL) {
		return;
	}
	CFURLRef url = CFURLCreateFromFileSystemRepresentation(NULL, (const UInt8 *)path, strlen(path), false);
	if (url == NULL) {
		out->status = errSecParam;
		CFRelease(r);
		return;
	}
	SecStaticCodeRef code = NULL;
	OSStatus st = SecStaticCodeCreateWithPath(url, kSecCSDefaultFlags, &code);
	CFRelease(url);
	if (st != errSecSuccess) {
		out->status = st;
		CFRelease(r);
		return;
	}
	st = SecStaticCodeCheckValidity(code, kSecCSDefaultFlags, r);
	CFRelease(r);
	if (st != errSecSuccess) {
		out->status = st;
		CFRelease(code);
		return;
	}
	strncpy(out->path, path, sizeof(out->path) - 1);
	orch_info(code, kSecCSSigningInformation, out);
	CFRelease(code);
}
*/
import "C"

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sigOf converts a C check result.
func sigOf(out *C.orch_sig) (codeSignature, error) {
	if out.status != 0 {
		return codeSignature{}, fmt.Errorf("code signature check failed (OSStatus %d)", int32(out.status))
	}
	n := int(out.cdhash_len)
	return codeSignature{
		Path:   C.GoString(&out.path[0]),
		Flags:  uint32(out.flags),
		CDHash: C.GoBytes(unsafe.Pointer(&out.cdhash[0]), C.int(n)),
	}, nil
}

// runningSig checks the process at the other end of c, by the
// socket's audit token (not its PID, which may be reused), against
// requirement and returns its signature.
func runningSig(c net.Conn, requirement string) (codeSignature, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return codeSignature{}, fmt.Errorf("%T has no socket to inspect", c)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return codeSignature{}, err
	}
	var tok auditToken
	var terr error
	if err := raw.Control(func(fd uintptr) {
		n := uint32(unsafe.Sizeof(tok))
		if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_LOCAL, unix.LOCAL_PEERTOKEN,
			uintptr(unsafe.Pointer(&tok)), uintptr(unsafe.Pointer(&n)), 0); e != 0 {
			terr = fmt.Errorf("read the peer's audit token: %w", e)
		}
	}); err != nil {
		return codeSignature{}, err
	}
	if terr != nil {
		return codeSignature{}, terr
	}
	creq := C.CString(requirement)
	defer C.free(unsafe.Pointer(creq))
	var out C.orch_sig
	C.orch_check_running((*C.uchar)(unsafe.Pointer(&tok[0])), C.int(unsafe.Sizeof(tok)), creq, &out)
	return sigOf(&out)
}

// staticSig checks the code on disk at path against requirement and
// returns its signature.
func staticSig(path, requirement string) (codeSignature, error) {
	cpath, creq := C.CString(path), C.CString(requirement)
	defer C.free(unsafe.Pointer(cpath))
	defer C.free(unsafe.Pointer(creq))
	var out C.orch_sig
	C.orch_check_static(cpath, creq, &out)
	return sigOf(&out)
}

func init() {
	runningSignature, staticSignature = runningSig, staticSig
}
