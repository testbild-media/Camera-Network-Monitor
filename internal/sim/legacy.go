package sim

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"golang.org/x/crypto/scrypt"
)

// Legacy fixture credentials (documented for upgrade tests).
const (
	LegacyAdminUser  = "chef"
	LegacyAdminPass  = "legacy-admin-1"
	LegacyDVRPass    = "dvr-geheim"
	LegacyKioskToken = "legacyKioskToken_0123456789abcdefghijklmnopqrstuv"
)

// WriteLegacyFixture writes a config.json + master.key in the style of an
// earlier Node.js based CNM (hex AES-GCM objects, "salt:hash" scrypt, SHA-256
// kiosk hashes) to dir. It exercises the tolerant migration path.
func WriteLegacyFixture(dir string, dahuaHTTP, dahuaRTSP int) error {
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	seal := func(pt string) map[string]string {
		iv := make([]byte, 12)
		rand.Read(iv)
		out := gcm.Seal(nil, iv, []byte(pt), nil)
		return map[string]string{"iv": hex.EncodeToString(iv), "tag": hex.EncodeToString(out[len(out)-16:]), "data": hex.EncodeToString(out[:len(out)-16])}
	}
	salt := hex.EncodeToString(key[:16])
	dk, _ := scrypt.Key([]byte(LegacyAdminPass), []byte(salt), 16384, 8, 1, 64)
	tokHash := sha256.Sum256([]byte(LegacyKioskToken))
	tokEnc := seal(LegacyKioskToken)
	cfg := map[string]any{
		"version": "0.3.25",
		"system": map[string]any{
			"name": "Altanlage", "listenPort": 8090, "healthIntervalMs": 30000,
			"media": map[string]any{"go2rtcUrl": "http://127.0.0.1:1984", "preloadMultiview": true},
		},
		"devices": []any{map[string]any{
			"id": "dvr1", "label": "Eingang", "manufacturer": "Lupus", "model": "LE808HD", "host": "127.0.0.1",
			"credentials":  map[string]any{"username": "admin", "password": seal(LegacyDVRPass)},
			"ports":        map[string]any{"http": dahuaHTTP, "rtsp": dahuaRTSP},
			"channelCount": 8,
			"channels": []any{
				map[string]any{"index": 0, "label": "Tor", "enabled": true, "streams": map[string]any{"main": true, "sub": true}, "view": map[string]any{"aspectRatio": "16:9", "objectFit": "fill", "multiviewSource": "sub-main-fallback"}},
			},
		}},
		"monitors": []any{map[string]any{"id": "mon1", "label": "Wand", "enabled": true, "layout": map[string]any{"template": "2x2", "gap": 4, "rows": []any{
			map[string]any{"id": "r1", "height": 1, "slots": []any{map[string]any{"id": "s1", "cameraId": "dvr1:0", "width": 1}, map[string]any{"id": "s2", "cameraId": "", "width": 1}}},
		}}}},
		"users": []any{map[string]any{
			"id": "u1", "username": LegacyAdminUser, "role": "admin", "enabled": true,
			"passwordHash": salt + ":" + hex.EncodeToString(dk), "createdAt": int64(1700000000000),
			"apiKeys": []any{map[string]any{"id": "k1", "label": "Lobby", "monitorId": "mon1", "createdAt": int64(1700000000000), "enabled": true,
				"hash": hex.EncodeToString(tokHash[:]), "tokenEnc": tokEnc}},
		}},
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "master.key"), key, 0o600)
}
