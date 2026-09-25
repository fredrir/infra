package enrollment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const KeyFile = "/run/secrets/tailscale-auth-key"

var roles = map[string]string{"control": "tag:platform-control", "worker": "tag:platform-worker", "volatile": "tag:platform-volatile"}
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{4,128}$`)
var authKeyPattern = regexp.MustCompile(`^tskey-auth-[A-Za-z0-9_-]{20,250}$`)

type Target struct{ Node, Role, Host, KeyFile, KeyID string }
type Transport func(context.Context, string, Target, map[string]any) error
type Client struct {
	API         API
	Transport   Transport
	Environment map[string]string
	Now         func() time.Time
}

func ValidateTarget(t Target) error {
	if !regexp.MustCompile(`^fredrir-[0-9]{2}$`).MatchString(t.Node) || roles[t.Role] == "" {
		return fmt.Errorf("concrete fleet node and role required")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`).MatchString(t.Host) {
		return fmt.Errorf("verified SSH host alias required")
	}
	if !regexp.MustCompile(`^/run/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_-]+$`).MatchString(t.KeyFile) || path.Clean(t.KeyFile) != t.KeyFile {
		return fmt.Errorf("private runtime key path under /run required")
	}
	return nil
}

func capabilities(role string) map[string]any {
	return map[string]any{"devices": map[string]any{"create": map[string]any{"reusable": false, "ephemeral": false, "preauthorized": true, "tags": []any{roles[role]}}}}
}

func validateMetadata(document map[string]any, role, description string, now time.Time, fresh bool) (map[string]any, error) {
	id, ok := document["id"].(string)
	if !ok || !keyIDPattern.MatchString(id) {
		return nil, fmt.Errorf("key identifier missing or malformed")
	}
	if !reflect.DeepEqual(document["capabilities"], capabilities(role)) || document["description"] != description {
		return nil, fmt.Errorf("key metadata differs from node and role contract")
	}
	createdText, cok := document["created"].(string)
	expiresText, eok := document["expires"].(string)
	created, cerr := time.Parse(time.RFC3339Nano, createdText)
	expires, eerr := time.Parse(time.RFC3339Nano, expiresText)
	if !cok || !eok || cerr != nil || eerr != nil {
		return nil, fmt.Errorf("key timestamps require timezone")
	}
	if lifetime := expires.Sub(created); lifetime < time.Second || lifetime > 605*time.Second {
		return nil, fmt.Errorf("key lifetime exceeds ten-minute contract")
	}
	invalid, hasInvalid := document["invalid"]
	revoked := document["revoked"]
	if fresh && ((hasInvalid && invalid != false) || (revoked != nil && revoked != "" && revoked != "0001-01-01T00:00:00Z") || now.Sub(created) < -60*time.Second || now.Sub(created) > 60*time.Second || expires.Sub(now) < 60*time.Second || expires.Sub(now) > 665*time.Second) {
		return nil, fmt.Errorf("key invalid, stale or outside clock skew")
	}
	return map[string]any{"id": id, "created": createdText, "expires": expiresText, "description": description, "capabilities": document["capabilities"]}, nil
}

func keyIDs(ctx context.Context, api API, token string) (map[string]bool, error) {
	document, err := api(ctx, Request{Method: "GET", Path: "/tailnet/-/keys", Token: token})
	if err != nil {
		return nil, err
	}
	value, present := document["keys"]
	if !present {
		return nil, fmt.Errorf("unexpected auth-key inventory")
	}
	result := map[string]bool{}
	if value == nil {
		return result, nil
	}
	keys, ok := value.([]any)
	if !ok || len(keys) > 1000 {
		return nil, fmt.Errorf("unexpected auth-key inventory")
	}
	for _, item := range keys {
		key, ok := item.(map[string]any)
		id, idOK := key["id"].(string)
		if !ok || !idOK || !keyIDPattern.MatchString(id) {
			return nil, fmt.Errorf("unexpected auth-key inventory")
		}
		result[id] = true
	}
	return result, nil
}

func (c Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c Client) CreateDeliver(ctx context.Context, target Target) (result map[string]any, resultErr error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	if target.KeyID != "" {
		return nil, fmt.Errorf("key ID accepted only for revocation")
	}
	if c.API == nil || c.Transport == nil {
		return nil, fmt.Errorf("enrollment clients required")
	}
	token, err := accessToken(ctx, c.API, target.Role, c.Environment)
	if err != nil {
		return nil, err
	}
	if err := c.Transport(ctx, "preflight", target, nil); err != nil {
		return nil, err
	}
	existing, err := keyIDs(ctx, c.API, token)
	if err != nil {
		return nil, err
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	description := "enroll-" + target.Node + "-" + target.Role + "-" + hex.EncodeToString(random[:])
	deliveryAttempted := false
	defer func() {
		if resultErr == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		var failures []string
		if target.KeyID != "" {
			if err := revoke(cleanup, c.API, token, target.KeyID); err != nil {
				failures = append(failures, "API revocation unconfirmed")
			}
		} else {
			current, err := keyIDs(cleanup, c.API, token)
			recovered := false
			if err == nil {
				ids := make([]string, 0, len(current))
				for id := range current {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				for _, id := range ids {
					if existing[id] {
						continue
					}
					item, getErr := c.API(cleanup, Request{Method: "GET", Path: "/tailnet/-/keys/" + id, Token: token})
					if getErr != nil {
						err = getErr
						continue
					}
					if item != nil && item["description"] == description {
						if e := revoke(cleanup, c.API, token, id); e != nil {
							err = e
						} else {
							recovered = true
						}
					}
				}
			}
			if err != nil || !recovered {
				failures = append(failures, "creation outcome and API revocation unconfirmed")
			}
		}
		if deliveryAttempted {
			if err := c.Transport(cleanup, "cleanup", target, nil); err != nil {
				failures = append(failures, "remote cleanup unconfirmed")
			}
		}
		detail := strings.Join(failures, "; ")
		if detail == "" {
			detail = "created key revoked; no usable delivered key retained"
		}
		resultErr = fmt.Errorf("enrollment key preparation failed; %s; request %s", detail, description)
	}()
	created, err := c.API(ctx, Request{Method: "POST", Path: "/tailnet/-/keys", Token: token, Document: map[string]any{"capabilities": capabilities(target.Role), "expirySeconds": 600, "description": description}})
	if err != nil {
		return nil, err
	}
	if id, ok := created["id"].(string); ok && keyIDPattern.MatchString(id) && !existing[id] {
		target.KeyID = id
	}
	if target.KeyID == "" {
		return nil, fmt.Errorf("API did not identify a new key")
	}
	metadata, err := validateMetadata(created, target.Role, description, c.now(), true)
	if err != nil {
		return nil, err
	}
	key, ok := created["key"].(string)
	if !ok || !authKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("API did not return node auth key")
	}
	verified, err := c.API(ctx, Request{Method: "GET", Path: "/tailnet/-/keys/" + target.KeyID, Token: token})
	if err != nil {
		return nil, err
	}
	checked, err := validateMetadata(verified, target.Role, description, c.now(), true)
	if err != nil || !reflect.DeepEqual(metadata, checked) {
		return nil, fmt.Errorf("created key metadata changed before delivery")
	}
	metadata["node"], metadata["role"] = target.Node, target.Role
	deliveryAttempted = true
	if err := c.Transport(ctx, "deliver", target, map[string]any{"key": key, "metadata": metadata}); err != nil {
		return nil, err
	}
	return map[string]any{"kind": "delivered-tailscale-key", "node": target.Node, "role": target.Role, "keyId": target.KeyID, "expires": metadata["expires"], "keyFile": target.KeyFile, "oneTime": true, "requires": "Run bootstrap immediately; revoke and remove an unused key"}, nil
}

func (c Client) RevokeUnused(ctx context.Context, target Target) (map[string]any, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	if !keyIDPattern.MatchString(target.KeyID) {
		return nil, fmt.Errorf("concrete key ID required")
	}
	if c.API == nil || c.Transport == nil {
		return nil, fmt.Errorf("enrollment clients required")
	}
	token, err := accessToken(ctx, c.API, target.Role, c.Environment)
	if err != nil {
		return nil, err
	}
	document, err := c.API(ctx, Request{Method: "GET", Path: "/tailnet/-/keys/" + target.KeyID, Token: token})
	if err != nil {
		return nil, err
	}
	if document != nil {
		if document["id"] != target.KeyID {
			return nil, fmt.Errorf("key response identity differs")
		}
		description, _ := document["description"].(string)
		if !regexp.MustCompile(`^enroll-` + regexp.QuoteMeta(target.Node) + `-` + target.Role + `-[a-f0-9]{24}$`).MatchString(description) {
			return nil, fmt.Errorf("key does not belong to bootstrap target")
		}
		if _, err := validateMetadata(document, target.Role, description, c.now(), false); err != nil {
			return nil, err
		}
		if err := revoke(ctx, c.API, token, target.KeyID); err != nil {
			return nil, err
		}
	}
	if err := c.Transport(ctx, "cleanup", target, nil); err != nil {
		return nil, err
	}
	return map[string]any{"kind": "revoked-tailscale-key", "node": target.Node, "role": target.Role, "keyId": target.KeyID, "runtimeFileRemoved": true}, nil
}

func revoke(ctx context.Context, api API, token, id string) error {
	_, err := api(ctx, Request{Method: "DELETE", Path: "/tailnet/-/keys/" + url.PathEscape(id), Token: token})
	return err
}
