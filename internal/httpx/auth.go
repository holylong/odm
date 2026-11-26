package httpx

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime/quotedprintable"
	"strconv"
	"strings"
)

// BuildAuthorization renders an Authorization / Proxy-Authorization value using
// the exact Digest template observed in IDMan.exe:
//
//	Digest username="%s", realm="%s", nonce="%s", uri="%s", %sresponse="%s"%s
//	  where the leading %s is:  qop=%s, nc=%s, cnonce="%s",
//	  and the trailing %s is:   , opaque="%s"
func BuildAuthorization(challenge, method, uri, user, pass, cnonce string) string {
	kind, params := parseChallenge(challenge)
	switch strings.ToLower(kind) {
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	case "digest":
		realm := params["realm"]
		nonce := params["nonce"]
		opaque := params["opaque"]
		algorithm := strings.ToUpper(params["algorithm"])
		qopRaw := params["qop"]

		ha1 := md5hex(user + ":" + realm + ":" + pass)
		if algorithm == "MD5-SESS" {
			ha1 = md5hex(ha1 + ":" + nonce + ":" + cnonce)
		}
		ha2 := md5hex(method + ":" + uri)

		var extra, tail string
		useQop := ""
		for _, q := range strings.Split(qopRaw, ",") {
			if strings.TrimSpace(q) == "auth" {
				useQop = "auth"
				break
			}
		}
		var response string
		if useQop == "auth" {
			nc := "00000001"
			extra = fmt.Sprintf("qop=%s, nc=%s, cnonce=%q, ", useQop, nc, cnonce)
			response = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + useQop + ":" + ha2)
		} else {
			response = md5hex(ha1 + ":" + nonce + ":" + ha2)
		}
		if opaque != "" {
			tail = fmt.Sprintf(", opaque=%q", opaque)
		}
		return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", %sresponse="%s"%s`,
			user, realm, nonce, uri, extra, response, tail)
	case "bearer":
		return "Bearer " + pass
	}
	// Unknown scheme: fall back to Basic, which most servers still accept.
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// parseChallenge splits `Scheme k="v", k2=v2` into a scheme and a map.
func parseChallenge(ch string) (string, map[string]string) {
	ch = strings.TrimSpace(ch)
	params := map[string]string{}
	sp := strings.IndexByte(ch, ' ')
	if sp < 0 {
		return ch, params
	}
	scheme := ch[:sp]
	rest := ch[sp+1:]
	i := 0
	for i < len(rest) {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == ',') {
			i++
		}
		eq := strings.IndexByte(rest[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[i : i+eq]))
		i += eq + 1
		var val string
		if i < len(rest) && rest[i] == '"' {
			i++
			start := i
			for i < len(rest) && rest[i] != '"' {
				i++
			}
			val = rest[start:i]
			if i < len(rest) {
				i++
			}
		} else {
			start := i
			for i < len(rest) && rest[i] != ',' {
				i++
			}
			val = strings.TrimSpace(rest[start:i])
		}
		params[key] = val
	}
	return scheme, params
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// RandomNonce returns a hex client nonce.
func RandomNonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// DecodeRFC2047 decodes =?charset?B?...?= / =?charset?Q?...?= filename encodings
// seen in the binary's Content-Disposition handling (re/FINDINGS.md §2).
func DecodeRFC2047(s string) string {
	if !strings.HasPrefix(s, "=?") {
		return s
	}
	end := strings.Index(s, "?=")
	if end < 0 {
		return s
	}
	body := s[2:end]
	parts := strings.SplitN(body, "?", 3)
	if len(parts) != 3 {
		return s
	}
	enc, payload := strings.ToUpper(parts[1]), parts[2]
	var raw []byte
	switch enc {
	case "B":
		b, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return s
		}
		raw = b
	case "Q":
		r := quotedprintable.NewReader(strings.NewReader(strings.ReplaceAll(payload, "_", " ")))
		buf := make([]byte, 0, len(payload))
		tmp := make([]byte, 512)
		for {
			n, err := r.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		raw = buf
	default:
		return s
	}
	return string(raw) + s[end+2:]
}

// ParseRetryAfter returns seconds to wait from a Retry-After header value.
func ParseRetryAfter(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return 0
}
