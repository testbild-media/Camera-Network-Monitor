// Package secret implements CNM's secret handling:
//
//   - the AES-256-GCM master key (stored in its own file, never inside config.json)
//   - reversible encryption of DVR passwords and recoverable kiosk tokens
//   - scrypt password hashing for CNM user accounts
//   - kiosk token generation and SHA-256 verification hashes
//
// Decoding is deliberately tolerant: configurations written by earlier CNM
// versions may use different encodings (hex/base64, string/object layouts).
// AES-GCM authenticates every candidate interpretation, so trying several
// layouts can never yield a wrong plaintext.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the AES-256 key length in bytes.
const KeySize = 32

// encPrefix marks values written by this implementation.
const encPrefix = "cnm1"

var (
	// ErrNoKey is returned when a secret must be decrypted but no master key is loaded.
	ErrNoKey = errors.New("secret: master key not loaded")
	// ErrUndecryptable is returned when no known layout authenticates with the master key.
	ErrUndecryptable = errors.New("secret: value cannot be decrypted with the current master key")
)

// Keyring wraps the master key. It is safe for concurrent use.
type Keyring struct {
	key []byte
}

// NewKeyring creates a keyring from a raw 32-byte key.
func NewKeyring(key []byte) (*Keyring, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret: master key must be %d bytes, got %d", KeySize, len(key))
	}
	k := make([]byte, KeySize)
	copy(k, key)
	return &Keyring{key: k}, nil
}

// GenerateKey returns a fresh random master key.
func GenerateKey() []byte {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		panic("secret: crypto/rand failed: " + err.Error())
	}
	return k
}

// ParseKeyFile accepts the master key as 32 raw bytes, 64 hex characters or
// base64 (standard or URL alphabet, padded or not). Surrounding whitespace is ignored
// for the text forms.
func ParseKeyFile(data []byte) ([]byte, error) {
	if len(data) == KeySize {
		return append([]byte(nil), data...), nil
	}
	s := strings.TrimSpace(string(data))
	if len(s) == 2*KeySize {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	// A JSON wrapper such as {"key":"..."} is accepted as a courtesy.
	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		for _, k := range []string{"key", "masterKey", "value"} {
			if v, ok := obj[k].(string); ok {
				return ParseKeyFile([]byte(v))
			}
		}
	}
	return nil, fmt.Errorf("secret: master key file has an unrecognised format (%d bytes)", len(data))
}

// LoadKeyFile reads and parses an existing master key file.
func LoadKeyFile(path string) (*Keyring, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := ParseKeyFile(data)
	if err != nil {
		return nil, err
	}
	return NewKeyring(key)
}

// CreateKeyFile writes a new random key with mode 0600. It refuses to overwrite.
func CreateKeyFile(path string) (*Keyring, error) {
	key := GenerateKey()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("secret: refusing to overwrite existing master key %s", path)
		}
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return NewKeyring(key)
}

// Encrypt seals plaintext with a random 96-bit IV. Output format:
//
//	cnm1:<base64 iv>:<base64 tag>:<base64 ciphertext>
func (k *Keyring) Encrypt(plaintext string) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	ct, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	b64 := base64.StdEncoding
	return strings.Join([]string{encPrefix, b64.EncodeToString(iv), b64.EncodeToString(tag), b64.EncodeToString(ct)}, ":"), nil
}

// IsCanonical reports whether a stored value already uses this implementation's format.
func IsCanonical(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	return strings.HasPrefix(s, encPrefix+":")
}

type candidate struct{ iv, tag, ct []byte }

// Decrypt opens a stored secret. raw is the JSON value as found in config.json:
// a string or an object. Several historic layouts are tried; GCM authentication
// guarantees that only the correct interpretation succeeds.
func (k *Keyring) Decrypt(raw json.RawMessage) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	cands := candidatesFromJSON(raw)
	for _, c := range cands {
		if pt, ok := k.open(c); ok {
			return pt, nil
		}
	}
	return "", ErrUndecryptable
}

func (k *Keyring) open(c candidate) (string, bool) {
	if len(c.tag) != 16 || (len(c.iv) != 12 && len(c.iv) != 16) {
		return "", false
	}
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(c.iv))
	if err != nil {
		return "", false
	}
	buf := make([]byte, 0, len(c.ct)+len(c.tag))
	buf = append(buf, c.ct...)
	buf = append(buf, c.tag...)
	pt, err := gcm.Open(nil, c.iv, buf, nil)
	if err != nil {
		return "", false
	}
	return string(pt), true
}

// decodings returns every plausible byte interpretation of an encoded string.
func decodings(s string) [][]byte {
	s = strings.TrimSpace(s)
	var out [][]byte
	if len(s)%2 == 0 {
		if b, err := hex.DecodeString(s); err == nil {
			out = append(out, b)
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			dup := false
			for _, o := range out {
				if string(o) == string(b) {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, b)
			}
		}
	}
	return out
}

func candidatesFromJSON(raw json.RawMessage) []candidate {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return candidatesFromString(s)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		return candidatesFromObject(obj)
	}
	return nil
}

func pick(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func candidatesFromObject(obj map[string]any) []candidate {
	iv := pick(obj, "iv", "nonce", "IV")
	tag := pick(obj, "tag", "authTag", "auth_tag", "mac")
	ct := pick(obj, "ciphertext", "data", "ct", "content", "encrypted", "value", "enc", "cipher")
	if iv == "" || ct == "" {
		// Possibly a wrapper such as {"enc": "<string format>"}.
		if ct != "" {
			return candidatesFromString(ct)
		}
		return nil
	}
	var out []candidate
	if tag == "" {
		// tag appended to ciphertext (WebCrypto style)
		for _, ivb := range decodings(iv) {
			for _, ctb := range decodings(ct) {
				if len(ctb) >= 16 {
					out = append(out, candidate{iv: ivb, tag: ctb[len(ctb)-16:], ct: ctb[:len(ctb)-16]})
				}
			}
		}
		return out
	}
	for _, ivb := range decodings(iv) {
		for _, tagb := range decodings(tag) {
			for _, ctb := range decodings(ct) {
				out = append(out, candidate{iv: ivb, tag: tagb, ct: ctb})
			}
		}
	}
	return out
}

func candidatesFromString(s string) []candidate {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []candidate
	for _, sep := range []string{":", ".", "$"} {
		parts := strings.Split(s, sep)
		if len(parts) < 3 {
			continue
		}
		// The last three segments carry the payload; leading segments are prefixes
		// such as "cnm1", "enc", "v1" or "aes-256-gcm".
		p := parts[len(parts)-3:]
		for _, a := range decodings(p[0]) {
			for _, b := range decodings(p[1]) {
				for _, c := range decodings(p[2]) {
					out = append(out,
						candidate{iv: a, tag: b, ct: c}, // iv:tag:ct (Node default ordering)
						candidate{iv: a, tag: c, ct: b}, // iv:ct:tag
					)
				}
			}
		}
	}
	// Single blob layouts: iv|tag|ct and iv|ct|tag with 12 or 16 byte IVs.
	for _, blob := range decodings(strings.TrimPrefix(s, encPrefix+":")) {
		for _, ivLen := range []int{12, 16} {
			if len(blob) < ivLen+16 {
				continue
			}
			rest := blob[ivLen:]
			out = append(out,
				candidate{iv: blob[:ivLen], tag: rest[:16], ct: rest[16:]},
				candidate{iv: blob[:ivLen], tag: rest[len(rest)-16:], ct: rest[:len(rest)-16]},
			)
		}
	}
	return out
}

// LooksEncrypted reports whether a stored value has the structure of an
// encrypted secret (as opposed to a legacy plaintext password).
func LooksEncrypted(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.HasPrefix(s, encPrefix+":") {
			return true
		}
		for _, sep := range []string{":", "."} {
			parts := strings.Split(s, sep)
			if len(parts) >= 3 {
				ok := 0
				for _, p := range parts[len(parts)-3:] {
					if len(decodings(p)) > 0 && len(p) >= 16 {
						ok++
					}
				}
				if ok >= 2 {
					return true
				}
			}
		}
		// Single blob (iv|tag|ct): long, no whitespace, decodes to >= 29 bytes.
		if len(s) >= 40 && !strings.ContainsAny(s, " 	") {
			for _, d := range decodings(s) {
				if len(d) >= 29 {
					return true
				}
			}
		}
		return false
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		return pick(obj, "iv", "nonce", "IV") != "" || pick(obj, "ciphertext", "data", "ct", "content", "encrypted", "enc") != ""
	}
	return false
}
