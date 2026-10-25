package argocd

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// maskKey makes the masked form of a Secret value stable within the process and
// useless outside it.
var maskKey = func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}()

// maskManifests hides the values of every Secret among manifests. The manifests
// of a render by revision come with the data of Secrets in the clear (Argo CD
// masks only what an upload renders), so this runs on the response before it
// leaves the client: nothing else ever sees a Secret value. A Secret's data and
// stringData values become a keyed hash, so a change of a value still shows as a
// change. A manifest that is not JSON is an error that says which one, never
// what it holds.
func maskManifests(manifests []string) ([]string, error) {
	out := make([]string, len(manifests))
	for i, m := range manifests {
		var obj map[string]any
		if err := json.Unmarshal([]byte(m), &obj); err != nil {
			return nil, fmt.Errorf("%w: manifest %d of the response is not a JSON object", ErrUnreachable, i)
		}
		if obj["kind"] != "Secret" || !isCoreAPIVersion(obj["apiVersion"]) {
			out[i] = m
			continue
		}
		for _, field := range []string{"data", "stringData"} {
			values, ok := obj[field].(map[string]any)
			if !ok {
				continue
			}
			for k, v := range values {
				mac := hmac.New(sha256.New, maskKey)
				_, _ = fmt.Fprint(mac, v)
				values[k] = "<hidden:" + hex.EncodeToString(mac.Sum(nil)[:8]) + ">"
			}
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("%w: manifest %d of the response cannot be encoded", ErrUnreachable, i)
		}
		out[i] = string(b)
	}
	return out, nil
}

func isCoreAPIVersion(v any) bool { return v == "v1" }

// maxServerMessage bounds the text of an error of Argo CD that is kept.
const maxServerMessage = 300

var quoted = regexp.MustCompile("`[^`]*`|\"[^\"]*\"|'[^']*'")

// sanitizeMessage makes the message of an Argo CD error safe to keep and to
// show: a render error can quote the manifest or the value it failed on, and a
// value may be a Secret's. Only the first line is kept, quoted text is dropped,
// and the rest is cut to a bounded length.
func sanitizeMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	msg = quoted.ReplaceAllString(msg, `"…"`)
	if r := []rune(msg); len(r) > maxServerMessage {
		msg = string(r[:maxServerMessage]) + "…"
	}
	return msg
}
