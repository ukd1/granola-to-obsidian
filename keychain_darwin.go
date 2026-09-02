//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework CoreFoundation -framework Security

#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>

static OSStatus granolaKeychainRead(
	const char *service,
	UInt32 serviceLength,
	const char *account,
	UInt32 accountLength,
	void **passwordData,
	UInt32 *passwordLength
) {
	return SecKeychainFindGenericPassword(
		NULL,
		serviceLength,
		service,
		accountLength,
		account,
		passwordLength,
		passwordData,
		NULL
	);
}

static void granolaKeychainFree(void *passwordData) {
	SecKeychainItemFreeContent(NULL, passwordData);
}

static OSStatus granolaKeychainWrite(
	const char *service,
	UInt32 serviceLength,
	const char *account,
	UInt32 accountLength,
	const void *passwordData,
	UInt32 passwordLength
) {
	SecKeychainItemRef item = NULL;
	OSStatus status = SecKeychainFindGenericPassword(
		NULL,
		serviceLength,
		service,
		accountLength,
		account,
		NULL,
		NULL,
		&item
	);

	if (status == errSecSuccess) {
		status = SecKeychainItemModifyAttributesAndData(
			item,
			NULL,
			passwordLength,
			passwordData
		);
		CFRelease(item);
		return status;
	}
	if (status != errSecItemNotFound) {
		return status;
	}

	return SecKeychainAddGenericPassword(
		NULL,
		serviceLength,
		service,
		accountLength,
		account,
		passwordLength,
		passwordData,
		NULL
	);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func readGranolaSyncKeychain(service, account string) ([]byte, error) {
	serviceBytes := []byte(service)
	accountBytes := []byte(account)
	var passwordData unsafe.Pointer
	var passwordLength C.UInt32

	status := C.granolaKeychainRead(
		(*C.char)(unsafe.Pointer(&serviceBytes[0])),
		C.UInt32(len(serviceBytes)),
		(*C.char)(unsafe.Pointer(&accountBytes[0])),
		C.UInt32(len(accountBytes)),
		&passwordData,
		&passwordLength,
	)
	if status != C.errSecSuccess {
		return nil, fmt.Errorf("OSStatus %d", int32(status))
	}
	defer C.granolaKeychainFree(passwordData)
	return C.GoBytes(passwordData, C.int(passwordLength)), nil
}

func writeGranolaSyncKeychain(service, account string, payload []byte) error {
	if service == "" || account == "" || len(payload) == 0 {
		return fmt.Errorf("service, account, and payload must not be empty")
	}
	serviceBytes := []byte(service)
	accountBytes := []byte(account)
	status := C.granolaKeychainWrite(
		(*C.char)(unsafe.Pointer(&serviceBytes[0])),
		C.UInt32(len(serviceBytes)),
		(*C.char)(unsafe.Pointer(&accountBytes[0])),
		C.UInt32(len(accountBytes)),
		unsafe.Pointer(&payload[0]),
		C.UInt32(len(payload)),
	)
	if status != C.errSecSuccess {
		return fmt.Errorf("OSStatus %d", int32(status))
	}
	return nil
}
