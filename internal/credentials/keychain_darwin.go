//go:build darwin && cgo

package credentials

/*
#cgo darwin LDFLAGS: -framework Security -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>

static CFStringRef aa_string(const char *value) {
	return CFStringCreateWithCString(NULL, value, kCFStringEncodingUTF8);
}

// aa_lookup_query builds the lookup for one item, which must never show UI:
// the collector runs in the background, where a prompt would hang it or
// surprise the user, so a Keychain that needs the user is reported instead
// (errSecInteractionNotAllowed).
//
// That takes kSecUseAuthenticationUIFail, deprecated since macOS 11 in favor
// of kSecUseAuthenticationContext with LAContext.interactionNotAllowed. The
// replacement does not cover these items: Security.framework's SecItem.h says
// it "has the same effect as passing kSecUseNoAuthenticationUI", which "on
// macOS ... only applies to items stored in the Data Protection keychain.
// Legacy keychain items will still activate UI if needed." These items live in
// the legacy (login) keychain, since the Data Protection keychain requires a
// signed binary with a keychain-access-group entitlement and a build from
// source has none. So the deprecated value stays, the one deprecation warning
// is silenced here and nowhere else, and TestKeychainLookupForbidsUI pins it.
static CFDictionaryRef aa_lookup_query(CFStringRef svc, CFStringRef acct) {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
	const void *keys[] = { kSecClass, kSecAttrService, kSecAttrAccount, kSecReturnData, kSecUseAuthenticationUI };
	const void *values[] = { kSecClassGenericPassword, svc, acct, kCFBooleanTrue, kSecUseAuthenticationUIFail };
#pragma clang diagnostic pop
	return CFDictionaryCreate(NULL, keys, values, 5, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
}

// aa_lookup_forbids_ui reports whether the lookup query refuses UI, for a test.
static int aa_lookup_forbids_ui(void) {
	CFStringRef svc = aa_string("service"), acct = aa_string("account");
	CFDictionaryRef query = aa_lookup_query(svc, acct);
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
	int forbids = CFEqual(CFDictionaryGetValue(query, kSecUseAuthenticationUI), kSecUseAuthenticationUIFail);
#pragma clang diagnostic pop
	CFRelease(query); CFRelease(svc); CFRelease(acct);
	return forbids;
}

static int aa_keychain_get(const char *service, const char *account, void **out, size_t *out_len) {
	CFStringRef svc = aa_string(service), acct = aa_string(account);
	CFDictionaryRef query = aa_lookup_query(svc, acct);
	CFTypeRef result = NULL;
	OSStatus status = SecItemCopyMatching(query, &result);
	CFRelease(query); CFRelease(svc); CFRelease(acct);
	if (status != errSecSuccess) return (int)status;
	CFDataRef data = (CFDataRef)result;
	CFIndex len = CFDataGetLength(data);
	void *copy = malloc((size_t)len);
	if (copy == NULL) { CFRelease(result); return (int)errSecAllocate; }
	memcpy(copy, CFDataGetBytePtr(data), (size_t)len);
	*out = copy; *out_len = (size_t)len;
	CFRelease(result);
	return 0;
}

static int aa_keychain_save(const char *service, const char *account, const void *bytes, size_t len) {
	CFStringRef svc = aa_string(service), acct = aa_string(account);
	CFDataRef data = CFDataCreate(NULL, bytes, (CFIndex)len);
	const void *keys[] = { kSecClass, kSecAttrService, kSecAttrAccount, kSecValueData };
	const void *values[] = { kSecClassGenericPassword, svc, acct, data };
	CFDictionaryRef item = CFDictionaryCreate(NULL, keys, values, 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	OSStatus status = SecItemAdd(item, NULL);
	CFRelease(item);
	if (status != errSecDuplicateItem) { CFRelease(data); CFRelease(svc); CFRelease(acct); return (int)status; }
	const void *queryKeys[] = { kSecClass, kSecAttrService, kSecAttrAccount };
	const void *queryValues[] = { kSecClassGenericPassword, svc, acct };
	CFDictionaryRef query = CFDictionaryCreate(NULL, queryKeys, queryValues, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	const void *changeKeys[] = { kSecValueData };
	const void *changeValues[] = { data };
	CFDictionaryRef changes = CFDictionaryCreate(NULL, changeKeys, changeValues, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	status = SecItemUpdate(query, changes);
	CFRelease(changes); CFRelease(query); CFRelease(data); CFRelease(svc); CFRelease(acct);
	return (int)status;
}

static int aa_keychain_delete(const char *service, const char *account) {
	CFStringRef svc = aa_string(service), acct = aa_string(account);
	const void *keys[] = { kSecClass, kSecAttrService, kSecAttrAccount };
	const void *values[] = { kSecClassGenericPassword, svc, acct };
	CFDictionaryRef query = CFDictionaryCreate(NULL, keys, values, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	OSStatus status = SecItemDelete(query);
	CFRelease(query); CFRelease(svc); CFRelease(acct);
	return status == errSecItemNotFound ? 0 : (int)status;
}

static void aa_keychain_free(void *value) { free(value); }
*/
import "C"

import (
	"context"
	"errors"
	"runtime"
	"unsafe"
)

// The Go-side result codes in keychain_errors.go must equal the framework's.
// Each array length is zero only when they match; any difference fails the
// build rather than silently mis-mapping a Keychain failure.
var (
	_ [C.errSecItemNotFound - osStatusItemNotFound]struct{}
	_ [osStatusItemNotFound - C.errSecItemNotFound]struct{}
	_ [C.errSecInteractionNotAllowed - osStatusInteractionNotAllowed]struct{}
	_ [osStatusInteractionNotAllowed - C.errSecInteractionNotAllowed]struct{}
	_ [C.errSecAuthFailed - osStatusAuthFailed]struct{}
	_ [osStatusAuthFailed - C.errSecAuthFailed]struct{}
)

// KeychainStore uses Security.framework directly. It never invokes the
// `security` command, which would expose a secret through argv or shell logs.
type KeychainStore struct{ service string }

// keychainAccess is every call KeychainStore makes into the login Keychain.
// Each returns an OSStatus.
type keychainAccess struct {
	get    func(service, account string) ([]byte, int)
	save   func(service, account string, data []byte) int
	delete func(service, account string) int
}

// securityFramework reaches the real login Keychain through
// Security.framework.
var securityFramework = keychainAccess{
	get: func(service, account string) ([]byte, int) {
		cService, cAccount := C.CString(service), C.CString(account)
		defer C.free(unsafe.Pointer(cService))
		defer C.free(unsafe.Pointer(cAccount))
		var data unsafe.Pointer
		var length C.size_t
		// kSecUseAuthenticationUIFail (see aa_lookup_query) keeps a
		// background process from ever prompting; a locked Keychain is
		// reported instead.
		if status := C.aa_keychain_get(cService, cAccount, &data, &length); status != 0 {
			return nil, int(status)
		}
		defer C.aa_keychain_free(data)
		return C.GoBytes(data, C.int(length)), 0
	},
	save: func(service, account string, data []byte) int {
		cService, cAccount := C.CString(service), C.CString(account)
		defer C.free(unsafe.Pointer(cService))
		defer C.free(unsafe.Pointer(cAccount))
		status := C.aa_keychain_save(cService, cAccount, unsafe.Pointer(&data[0]), C.size_t(len(data)))
		runtime.KeepAlive(data)
		return int(status)
	},
	delete: func(service, account string) int {
		cService, cAccount := C.CString(service), C.CString(account)
		defer C.free(unsafe.Pointer(cService))
		defer C.free(unsafe.Pointer(cAccount))
		// aa_keychain_delete already treats an absent item as deleted.
		return int(C.aa_keychain_delete(cService, cAccount))
	},
}

// keychain is what every KeychainStore calls: securityFramework, except in
// this package's tests, whose TestMain replaces it with calls that stop the
// test, so only the opt-in TestKeychainRoundTrip reaches the real Keychain.
var keychain = securityFramework

// lookupForbidsUI reports whether Load's Keychain query refuses to show UI.
func lookupForbidsUI() bool { return C.aa_lookup_forbids_ui() != 0 }

// NewKeychainStore returns a store for generic-password items under service,
// ordinarily KeychainService.
func NewKeychainStore(service string) (*KeychainStore, error) {
	if service == "" {
		return nil, errors.New("keychain service is required")
	}
	return &KeychainStore{service: service}, nil
}

// Save stores value under reference, replacing any existing item.
func (s *KeychainStore) Save(ctx context.Context, reference string, value R2Credentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reference == "" {
		return ErrInvalidReference
	}
	encoded, err := EncodeSecret(value)
	if err != nil {
		return err
	}
	return errorForOSStatus(keychain.save(s.service, reference, encoded))
}

// Load returns the credentials stored under reference without ever showing
// UI: a Keychain that would need the user fails with ErrKeychainLocked.
func (s *KeychainStore) Load(ctx context.Context, reference string) (R2Credentials, error) {
	if err := ctx.Err(); err != nil {
		return R2Credentials{}, err
	}
	if reference == "" {
		return R2Credentials{}, ErrInvalidReference
	}
	data, status := keychain.get(s.service, reference)
	if err := errorForOSStatus(status); err != nil {
		return R2Credentials{}, err
	}
	return DecodeSecret(data)
}

// Delete removes the item under reference; an absent item is not an error.
func (s *KeychainStore) Delete(ctx context.Context, reference string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reference == "" {
		return ErrInvalidReference
	}
	return errorForOSStatus(keychain.delete(s.service, reference))
}

var _ CredentialStore = (*KeychainStore)(nil)
