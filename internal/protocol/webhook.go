package protocol

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

type encryptedWebhook struct {
	Encrypt string `json:"encrypt"`
}

// EncryptWebhook wraps payload in a Feishu encrypted webhook JSON envelope.
func EncryptWebhook(key string, payload []byte) ([]byte, error) {
	block := webhookBlock(key)
	padding := aes.BlockSize - len(payload)%aes.BlockSize
	raw := make([]byte, aes.BlockSize+len(payload)+padding)
	iv, data := raw[:aes.BlockSize], raw[aes.BlockSize:]
	_, _ = rand.Read(iv)
	copy(data, payload)
	copy(data[len(payload):], bytes.Repeat([]byte{byte(padding)}, padding))
	// #nosec G407 -- Feishu's CBC format prefixes a fresh random IV.
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	return json.Marshal(encryptedWebhook{Encrypt: base64.StdEncoding.EncodeToString(raw)})
}

// DecryptWebhook opens a Feishu encrypted webhook JSON envelope. Call
// VerifyWebhookSignature on the original body before decrypting a request.
func DecryptWebhook(key string, body []byte) ([]byte, error) {
	var envelope encryptedWebhook
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode webhook envelope: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(envelope.Encrypt)
	if err != nil {
		return nil, fmt.Errorf("decode webhook ciphertext: %w", err)
	}
	if len(raw) < 2*aes.BlockSize || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("invalid webhook ciphertext length")
	}
	iv, data := raw[:aes.BlockSize], raw[aes.BlockSize:]
	// #nosec G407 -- The IV is the first block of the signed Feishu payload.
	cipher.NewCBCDecrypter(webhookBlock(key), iv).CryptBlocks(data, data)
	return unpadWebhook(data)
}

func webhookBlock(key string) cipher.Block {
	digest := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		panic(err) // SHA-256 always produces a valid 32-byte AES key.
	}
	return block
}

func unpadWebhook(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("invalid PKCS7 padding")
	}
	size := int(data[len(data)-1])
	if size == 0 || size > aes.BlockSize || size > len(data) {
		return nil, errors.New("invalid PKCS7 padding")
	}
	unpadded := len(data) - size
	for _, value := range data[unpadded:] {
		if int(value) != size {
			return nil, errors.New("invalid PKCS7 padding")
		}
	}
	return data[:unpadded], nil
}

// WebhookSignature computes the X-Lark-Signature for the original raw body.
func WebhookSignature(timestamp, nonce, key string, body []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(timestamp + nonce + key))
	_, _ = hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}

// VerifyWebhookSignature compares the supplied signature in constant time.
func VerifyWebhookSignature(signature, timestamp, nonce, key string, body []byte) bool {
	expected := WebhookSignature(timestamp, nonce, key, body)
	return subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) == 1
}
