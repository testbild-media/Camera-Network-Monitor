package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/scrypt"
)

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k, _ := NewKeyring(GenerateKey())
	enc, err := k.Encrypt("geheim:pa$$wört")
	if err != nil {
		t.Fatal(err)
	}
	if !IsCanonical(mustJSON(enc)) {
		t.Fatalf("expected canonical format, got %q", enc)
	}
	pt, err := k.Decrypt(mustJSON(enc))
	if err != nil || pt != "geheim:pa$$wört" {
		t.Fatalf("roundtrip failed: %q %v", pt, err)
	}
	other, _ := NewKeyring(GenerateKey())
	if _, err := other.Decrypt(mustJSON(enc)); err == nil {
		t.Fatal("decrypt with wrong key must fail")
	}
}

// legacySeal simulates layouts an earlier (Node.js) implementation may have written.
func legacySeal(t *testing.T, key []byte, ivLen int, pt string) (iv, tag, ct []byte) {
	t.Helper()
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCMWithNonceSize(block, ivLen)
	iv = make([]byte, ivLen)
	for i := range iv {
		iv[i] = byte(i + 7)
	}
	sealed := gcm.Seal(nil, iv, []byte(pt), nil)
	return iv, sealed[len(sealed)-16:], sealed[:len(sealed)-16]
}

func TestDecryptLegacyLayouts(t *testing.T) {
	key := GenerateKey()
	k, _ := NewKeyring(key)
	h := hex.EncodeToString
	b := base64.StdEncoding.EncodeToString
	for _, ivLen := range []int{12, 16} {
		iv, tag, ct := legacySeal(t, key, ivLen, "dvr-pass")
		cases := map[string]json.RawMessage{
			"hex iv:tag:ct":         mustJSON(h(iv) + ":" + h(tag) + ":" + h(ct)),
			"b64 iv:tag:ct":         mustJSON(b(iv) + ":" + b(tag) + ":" + b(ct)),
			"prefixed enc:v1":       mustJSON("enc:v1:" + b(iv) + ":" + b(tag) + ":" + b(ct)),
			"iv:ct:tag hex":         mustJSON(h(iv) + ":" + h(ct) + ":" + h(tag)),
			"object hex":            mustJSON(map[string]string{"iv": h(iv), "tag": h(tag), "data": h(ct)}),
			"object b64 authTag":    mustJSON(map[string]string{"iv": b(iv), "authTag": b(tag), "ciphertext": b(ct)}),
			"blob iv|tag|ct base64": mustJSON(b(append(append(append([]byte{}, iv...), tag...), ct...))),
			"blob iv|ct|tag hex":    mustJSON(h(append(append(append([]byte{}, iv...), ct...), tag...))),
			"object tag appended":   mustJSON(map[string]string{"iv": b(iv), "data": b(append(append([]byte{}, ct...), tag...))}),
		}
		for name, raw := range cases {
			pt, err := k.Decrypt(raw)
			if err != nil || pt != "dvr-pass" {
				t.Errorf("ivLen=%d %s: got %q, %v", ivLen, name, pt, err)
			}
			if !LooksEncrypted(raw) {
				t.Errorf("ivLen=%d %s: LooksEncrypted=false", ivLen, name)
			}
		}
	}
	if LooksEncrypted(mustJSON("admin123")) {
		t.Error("plain password must not look encrypted")
	}
}

func TestKeyFileFormats(t *testing.T) {
	dir := t.TempDir()
	key := GenerateKey()
	for name, content := range map[string][]byte{
		"raw":    key,
		"hex":    []byte(hex.EncodeToString(key) + "\n"),
		"base64": []byte(base64.StdEncoding.EncodeToString(key)),
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, content, 0o600)
		k, err := LoadKeyFile(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(k.key) != string(key) {
			t.Fatalf("%s: key mismatch", name)
		}
	}
	p := filepath.Join(dir, "new.key")
	if _, err := CreateKeyFile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateKeyFile(p); err == nil {
		t.Fatal("CreateKeyFile must refuse to overwrite")
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	ok, rehash := VerifyPassword(mustJSON(h), "correct horse")
	if !ok || rehash {
		t.Fatalf("verify canonical: ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := VerifyPassword(mustJSON(h), "wrong horse"); ok {
		t.Fatal("wrong password accepted")
	}
	if ok, _ := VerifyPassword(nil, "x"); ok {
		t.Fatal("empty hash accepted")
	}
}

func TestLegacyNodeScryptFormats(t *testing.T) {
	saltHex := "0123456789abcdef0123456789abcdef"
	// Node: crypto.scryptSync(password, saltHexString, 64) – salt used as string bytes
	dk, _ := scrypt.Key([]byte("legacyPass1"), []byte(saltHex), 16384, 8, 1, 64)
	saltBytes, _ := hex.DecodeString(saltHex)
	dk2, _ := scrypt.Key([]byte("legacyPass1"), saltBytes, 16384, 8, 1, 64)
	cases := map[string]json.RawMessage{
		"salt:hash hex":         mustJSON(saltHex + ":" + hex.EncodeToString(dk)),
		"scrypt$salt$hash":      mustJSON("scrypt$" + saltHex + "$" + hex.EncodeToString(dk)),
		"scrypt:N:r:p:salt:key": mustJSON("scrypt:16384:8:1:" + saltHex + ":" + hex.EncodeToString(dk)),
		"decoded salt b64 key":  mustJSON(saltHex + ":" + base64.StdEncoding.EncodeToString(dk2)),
		"object":                mustJSON(map[string]any{"salt": saltHex, "hash": hex.EncodeToString(dk), "N": 16384}),
	}
	for name, raw := range cases {
		ok, rehash := VerifyPassword(raw, "legacyPass1")
		if !ok || !rehash {
			t.Errorf("%s: ok=%v rehash=%v", name, ok, rehash)
		}
		if ok, _ := VerifyPassword(raw, "legacyPass2"); ok {
			t.Errorf("%s: wrong password accepted", name)
		}
	}
}

func TestTokens(t *testing.T) {
	tok := GenerateToken()
	if len(tok) < 40 {
		t.Fatalf("token too short: %q", tok)
	}
	h := HashToken(tok)
	if !VerifyTokenHash(h, tok) || VerifyTokenHash(h, tok+"x") {
		t.Fatal("token hash verification broken")
	}
	raw, _ := hex.DecodeString(h)
	if !VerifyTokenHash(base64.StdEncoding.EncodeToString(raw), tok) {
		t.Fatal("base64 stored hash not accepted")
	}
}
