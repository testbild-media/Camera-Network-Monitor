package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// Password hashing parameters for new hashes. N=2^15, r=8 uses 32 MiB per
// verification, which is acceptable because logins are rate limited.
const (
	scryptN      = 1 << 15
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 32
	saltLen      = 16

	// MinPasswordLength is enforced for all CNM user passwords.
	MinPasswordLength = 8
)

var kvParams = regexp.MustCompile(`^[A-Za-z]+=\d+(,[A-Za-z]+=\d+)*$`)

// Legacy defaults (Node.js crypto.scrypt defaults).
const (
	legacyN = 16384
	legacyR = 8
	legacyP = 1
)

// HashPassword returns "scrypt$N$r$p$<b64 salt>$<b64 key>".
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s", scryptN, scryptR, scryptP, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

type scryptParams struct {
	n, r, p int
}

type hashCandidate struct {
	params scryptParams
	salt   []byte
	key    []byte
}

// VerifyPassword checks password against a stored hash. stored is the raw JSON
// value from config.json (string or object). needsRehash is true when the hash
// was accepted but is not in the current canonical format/strength.
func VerifyPassword(stored json.RawMessage, password string) (ok bool, needsRehash bool) {
	cands, canonical := parseStoredHash(stored)
	if len(cands) == 0 {
		// Burn comparable time so the absence of a hash is not observable.
		_, _ = scrypt.Key([]byte(password), []byte("cnm-dummy-salt-0"), legacyN, legacyR, legacyP, scryptKeyLen)
		return false, false
	}
	for _, c := range cands {
		if c.params.n <= 1 || c.params.n&(c.params.n-1) != 0 || c.params.n > 1<<20 || c.params.r <= 0 || c.params.p <= 0 || len(c.key) < 16 || len(c.key) > 128 {
			continue
		}
		dk, err := scrypt.Key([]byte(password), c.salt, c.params.n, c.params.r, c.params.p, len(c.key))
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(dk, c.key) == 1 {
			strong := c.params.n >= scryptN && c.params.r >= scryptR
			return true, !(canonical && strong)
		}
	}
	return false, false
}

// HasUsableHash reports whether the stored value can be interpreted as a password hash.
func HasUsableHash(stored json.RawMessage) bool {
	c, _ := parseStoredHash(stored)
	return len(c) > 0
}

func parseStoredHash(raw json.RawMessage) (cands []hashCandidate, canonical bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return parseHashString(s)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		return parseHashObject(obj), false
	}
	return nil, false
}

func parseHashString(s string) ([]hashCandidate, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	// Canonical: scrypt$N$r$p$salt$key (raw std base64)
	if strings.HasPrefix(s, "scrypt$") {
		parts := strings.Split(s, "$")
		if len(parts) == 6 {
			n, e1 := strconv.Atoi(parts[1])
			r, e2 := strconv.Atoi(parts[2])
			p, e3 := strconv.Atoi(parts[3])
			salt, e4 := base64.RawStdEncoding.DecodeString(parts[4])
			key, e5 := base64.RawStdEncoding.DecodeString(parts[5])
			if e1 == nil && e2 == nil && e3 == nil && e4 == nil && e5 == nil {
				return []hashCandidate{{params: scryptParams{n, r, p}, salt: salt, key: key}}, true
			}
		}
	}
	// Generic legacy layouts: [algo][N r p [keylen]] salt key, separated by $ or :
	var parts []string
	for _, sep := range []string{"$", ":", "."} {
		if strings.Contains(s, sep) {
			parts = strings.FieldsFunc(s, func(r rune) bool { return string(r) == sep })
			break
		}
	}
	if len(parts) < 2 {
		return nil, false
	}
	params := scryptParams{legacyN, legacyR, legacyP}
	var nums []int
	var rest []string
	for i, p := range parts {
		lp := strings.ToLower(p)
		if i == 0 && (lp == "scrypt" || lp == "s" || strings.HasPrefix(lp, "scrypt")) {
			continue
		}
		// key=value parameter notation (e.g. "N=16384,r=8,p=1")
		if kvParams.MatchString(p) {
			for _, kv := range strings.Split(p, ",") {
				k, v, _ := strings.Cut(kv, "=")
				n, err := strconv.Atoi(v)
				if err != nil {
					continue
				}
				switch strings.ToLower(k) {
				case "n", "cost", "ln":
					if strings.ToLower(k) == "ln" {
						n = 1 << n
					}
					params.n = n
				case "r", "blocksize":
					params.r = n
				case "p", "parallelization":
					params.p = n
				}
			}
			continue
		}
		if n, err := strconv.Atoi(p); err == nil && len(p) <= 7 {
			nums = append(nums, n)
			continue
		}
		rest = append(rest, p)
	}
	if len(nums) >= 1 {
		params.n = nums[0]
	}
	if len(nums) >= 2 {
		params.r = nums[1]
	}
	if len(nums) >= 3 {
		params.p = nums[2]
	}
	if len(rest) < 2 {
		return nil, false
	}
	salt, key := rest[len(rest)-2], rest[len(rest)-1]
	return expandCandidates(params, salt, key), false
}

func parseHashObject(obj map[string]any) []hashCandidate {
	params := scryptParams{legacyN, legacyR, legacyP}
	num := func(keys ...string) (int, bool) {
		for _, k := range keys {
			switch v := obj[k].(type) {
			case float64:
				return int(v), true
			case string:
				if n, err := strconv.Atoi(v); err == nil {
					return n, true
				}
			}
		}
		return 0, false
	}
	if n, ok := num("N", "n", "cost"); ok {
		params.n = n
	}
	if r, ok := num("r", "blockSize"); ok {
		params.r = r
	}
	if p, ok := num("p", "parallelization"); ok {
		params.p = p
	}
	// nested params object
	if po, ok := obj["params"].(map[string]any); ok {
		if v, ok := po["N"].(float64); ok {
			params.n = int(v)
		}
		if v, ok := po["r"].(float64); ok {
			params.r = int(v)
		}
		if v, ok := po["p"].(float64); ok {
			params.p = int(v)
		}
	}
	salt := pick(obj, "salt", "s")
	key := pick(obj, "hash", "key", "derivedKey", "dk", "digest")
	if salt == "" || key == "" {
		return nil
	}
	return expandCandidates(params, salt, key)
}

// expandCandidates enumerates salt/key interpretations: the salt may have been
// used as its literal string bytes (common in Node.js code that passes the hex
// string to scrypt) or in decoded form.
func expandCandidates(params scryptParams, salt, key string) []hashCandidate {
	saltForms := [][]byte{[]byte(salt)}
	for _, d := range decodings(salt) {
		saltForms = append(saltForms, d)
	}
	var out []hashCandidate
	for _, k := range decodings(key) {
		if len(k) < 16 {
			continue
		}
		for _, sf := range saltForms {
			out = append(out, hashCandidate{params: params, salt: sf, key: k})
		}
	}
	return out
}

// ValidatePasswordPolicy enforces the minimum password rules.
func ValidatePasswordPolicy(pw string) error {
	if len([]rune(pw)) < MinPasswordLength {
		return fmt.Errorf("Passwort muss mindestens %d Zeichen lang sein", MinPasswordLength)
	}
	if len(pw) > 1024 {
		return fmt.Errorf("Passwort ist zu lang")
	}
	return nil
}

// GenerateToken returns a URL-safe random bearer token with 256 bits of entropy.
func GenerateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secret: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomID returns a short random identifier (hex) of n bytes.
func RandomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("secret: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// HashToken returns the hex SHA-256 verification hash of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyTokenHash compares token against a stored SHA-256 hash (hex or base64) in constant time.
func VerifyTokenHash(storedHash, token string) bool {
	if storedHash == "" || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	for _, d := range decodings(storedHash) {
		if len(d) == len(sum) && subtle.ConstantTimeCompare(d, sum[:]) == 1 {
			return true
		}
	}
	return false
}
