package storage

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
)

func sha256Sum(data []byte) [32]byte { return sha256.Sum256(data) }

// md5Hex is the ETag an S3-compatible store reports for a single-part,
// non-KMS object. It is an identity for ETag comparison, not a security hash.
func md5Hex(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	const hex = "0123456789abcdef"
	result := make([]byte, len(sum)*2)
	for i, b := range sum {
		result[i*2] = hex[b>>4]
		result[i*2+1] = hex[b&15]
	}
	return string(result)
}
