package policy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

const objectStore = "platform/components/object-store"

var objectStoreCells = map[string]string{"hel1": "fredrir-04"}

var objectStoreGRPC = map[string][2]string{
	"server":      {"MASTER VOLUME FILER S3 CLIENT", "MASTER VOLUME FILER S3"},
	"admin":       {"ADMIN CLIENT", "ADMIN"},
	"worker":      {"WORKER CLIENT", ""},
	"meta-backup": {"CLIENT", ""},
}

var objectStorePorts = map[string]string{"server": "8333,9327", "worker": "9328"}

func lookup(v any, path ...string) any {
	for _, key := range path {
		fields, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = fields[key]
	}
	return v
}

func objectStoreResources(t *testing.T) []object {
	return renderedTree(t, objectStore, objectStore)
}

func TestObjectStoreCellsRunHardenedOnTheirDataNode(t *testing.T) {
	image := at(load(t, "platform/versions.yaml"), "images", "seaweedfs")
	resources := objectStoreResources(t)
	literals := map[string]map[string]string{}
	for _, resource := range resources {
		if resource["kind"] == "ConfigMap" {
			values := map[string]string{}
			for key, value := range resource["data"].(object) {
				values[key] = fmt.Sprint(value)
			}
			literals[at(resource, "metadata", "name").(string)] = values
		}
	}
	cells := map[string]bool{}
	for _, resource := range resources {
		if resource["kind"] != "StatefulSet" {
			continue
		}
		name := strings.TrimPrefix(at(resource, "metadata", "name").(string), "seaweedfs-")
		cells[name] = true
		spec := at(resource, "spec", "template", "spec").(object)
		if at(spec, "nodeSelector", "kubernetes.io/hostname") != objectStoreCells[name] || spec["tolerations"] != nil || spec["affinity"] != nil {
			t.Errorf("%s may leave %s", name, objectStoreCells[name])
		}
		if spec["automountServiceAccountToken"] != false || at(spec, "securityContext", "runAsNonRoot") != true || at(spec, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
			t.Errorf("%s pod is not hardened", name)
		}
		projected := map[string]map[string]bool{}
		for _, volume := range spec["volumes"].([]any) {
			files := map[string]bool{}
			if sources, ok := lookup(volume, "projected", "sources").([]any); ok {
				for _, source := range sources {
					for _, kind := range []string{"configMap", "secret"} {
						if items, ok := lookup(source, kind, "items").([]any); ok {
							for _, item := range items {
								files[at(item, "path").(string)] = true
							}
						}
					}
				}
			}
			projected[at(volume, "name").(string)] = files
		}
		for _, item := range spec["containers"].([]any) {
			container := item.(object)
			role := container["name"].(string)
			label := name + "/" + role
			if container["image"] != image {
				t.Errorf("%s runs %v instead of the pinned %v", label, container["image"], image)
			}
			if at(container, "securityContext", "readOnlyRootFilesystem") != true || at(container, "securityContext", "allowPrivilegeEscalation") != false || fmt.Sprint(at(container, "securityContext", "capabilities", "drop")) != "[ALL]" {
				t.Errorf("%s is not hardened", label)
			}
			if !memoryLimitAboveGoLimit(container) {
				t.Errorf("%s needs GOMEMLIMIT below its memory limit", label)
			}
			if fmt.Sprint(container["command"]) != "[/bin/sh /etc/seaweedfs/entrypoint/weed.sh]" {
				t.Errorf("%s bypasses the gRPC security precheck: %v", label, container["command"])
			}
			arguments := container["args"].([]any)
			if len(arguments) == 0 || arguments[0] != "-logtostderr=true" {
				t.Errorf("%s logs to files instead of stderr", label)
			}
			args := fmt.Sprint(arguments)
			for _, argument := range arguments {
				for _, flag := range []string{"-dataDir=", "-workingDir="} {
					if value, ok := strings.CutPrefix(fmt.Sprint(argument), flag); ok && (value == "/tmp" || strings.HasPrefix(value, "/tmp/")) {
						t.Errorf("%s writes %s into /tmp", label, flag)
					}
				}
			}
			mounts := map[string]string{}
			for _, mount := range container["volumeMounts"].([]any) {
				mounts[at(mount, "mountPath").(string)] = at(mount, "name").(string)
			}
			if mounts["/etc/seaweedfs/entrypoint"] != "entrypoint" {
				t.Errorf("%s lacks the entrypoint", label)
			}
			if _, ok := mounts["/tmp"]; ok && role != "server" {
				t.Errorf("%s mounts a writable /tmp", label)
			}
			environment := map[string]string{}
			if sources, ok := container["envFrom"].([]any); ok {
				for _, source := range sources {
					if reference, ok := lookup(source, "configMapRef", "name").(string); ok {
						for key, value := range literals[reference] {
							environment[key] = value
						}
					}
				}
			}
			for _, env := range container["env"].([]any) {
				if value, ok := at(env, "value").(string); ok {
					environment[at(env, "name").(string)] = value
				}
			}
			grpc, known := objectStoreGRPC[role]
			if !known {
				t.Errorf("%s has no declared gRPC role", label)
				continue
			}
			if environment["WEED_GRPC_COMPONENTS"] != grpc[0] || environment["WEED_GRPC_SERVED"] != grpc[1] {
				t.Errorf("%s gRPC precheck covers %q/%q instead of %q/%q", label, environment["WEED_GRPC_COMPONENTS"], environment["WEED_GRPC_SERVED"], grpc[0], grpc[1])
			}
			required := []string{"WEED_GRPC_CA"}
			for _, component := range strings.Fields(grpc[0]) {
				required = append(required, "WEED_GRPC_"+component+"_CERT", "WEED_GRPC_"+component+"_KEY")
			}
			for _, key := range required {
				file := environment[key]
				directory, base := path.Split(file)
				volume, mounted := mounts[strings.TrimSuffix(directory, "/")]
				if !mounted || !projected[volume][base] {
					t.Errorf("%s sets %s to %q, which is not a mounted projected file", label, key, file)
				}
			}
			for _, component := range strings.Fields(grpc[1]) {
				if environment["WEED_GRPC_"+component+"_ALLOWED_COMMONNAMES"] != "seaweedfs-"+name+"-internal" {
					t.Errorf("%s serves %s gRPC without the cell's client name allow-list", label, component)
				}
			}
			var ports []string
			if list, ok := container["ports"].([]any); ok {
				for _, port := range list {
					ports = append(ports, fmt.Sprint(at(port, "containerPort")))
				}
			}
			if strings.Join(ports, ",") != objectStorePorts[role] {
				t.Errorf("%s exposes %v", label, ports)
			}
			switch role {
			case "server":
				secrets := map[string]string{}
				for _, env := range container["env"].([]any) {
					if reference, ok := lookup(env, "valueFrom", "secretKeyRef").(object); ok {
						secrets[at(env, "name").(string)] = fmt.Sprint(reference["name"]) + "/" + fmt.Sprint(reference["key"])
					}
				}
				for _, key := range []string{"WEED_JWT_SIGNING_KEY", "WEED_JWT_SIGNING_READ_KEY", "WEED_JWT_FILER_SIGNING_KEY", "WEED_S3_SSE_KEK", "WEED_S3_SSE_KEK_PASSPHRASE"} {
					if secrets[key] != "seaweedfs-"+name+"/"+key {
						t.Errorf("%s takes %s from %q instead of its cell secret", label, key, secrets[key])
					}
				}
				for _, required := range []string{"-ip=127.0.0.1", "-ip.bind=127.0.0.1", "-master.telemetry=false", "-s3.port.iceberg=0", "-s3.port.lance=0", "-s3.cert.file=", "-s3.config=", "-dataCenter=" + name} {
					if !strings.Contains(args, required) {
						t.Errorf("%s lacks %s", label, required)
					}
				}
			case "worker":
				if !strings.Contains(args, "-metricsPort=9328") {
					t.Errorf("%s exports no lifecycle metrics", label)
				}
			default:
				if strings.Contains(args, "-ip=") && !strings.Contains(args, "-ip=127.0.0.1") {
					t.Errorf("%s listens beyond loopback", label)
				}
			}
		}
	}
	if len(cells) != len(objectStoreCells) {
		t.Fatalf("object store cells: %v", cells)
	}
}

func TestObjectStoreEntrypointRefusesPartialGRPCSecurity(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "weed.sh"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	entrypoint := filepath.Join(directory, "weed.sh")
	if err := os.WriteFile(entrypoint, bytes.Replace(script, []byte("exec /usr/bin/weed"), []byte("exec echo started"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(directory, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	complete := map[string]string{"WEED_GRPC_COMPONENTS": "MASTER S3 CLIENT", "WEED_GRPC_SERVED": "MASTER S3", "WEED_GRPC_MASTER_ALLOWED_COMMONNAMES": "seaweedfs-hel1-internal", "WEED_GRPC_S3_ALLOWED_COMMONNAMES": "seaweedfs-hel1-internal"}
	var files []string
	for _, key := range []string{"WEED_GRPC_CA", "WEED_GRPC_MASTER_CERT", "WEED_GRPC_MASTER_KEY", "WEED_GRPC_S3_CERT", "WEED_GRPC_S3_KEY", "WEED_GRPC_CLIENT_CERT", "WEED_GRPC_CLIENT_KEY"} {
		file := filepath.Join(directory, key)
		if err := os.WriteFile(file, []byte("pem"), 0o600); err != nil {
			t.Fatal(err)
		}
		complete[key] = file
		files = append(files, key)
	}
	run := func(environment map[string]string) (string, error) {
		command := exec.Command("/bin/sh", entrypoint, "-logtostderr=true", "server")
		command.Env = []string{"PATH=/usr/bin:/bin"}
		for key, value := range environment {
			command.Env = append(command.Env, key+"="+value)
		}
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if output, err := run(complete); err != nil || strings.TrimSpace(output) != "started -logtostderr=true server" {
		t.Fatalf("complete gRPC security refused: %v %s", err, output)
	}
	refuses := func(label string, environment map[string]string) {
		output, err := run(environment)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || strings.Contains(output, "started") {
			t.Errorf("%s: entrypoint started weed: %v %s", label, err, output)
		}
	}
	for key := range complete {
		if key == "WEED_GRPC_SERVED" {
			continue
		}
		environment := map[string]string{}
		for name, value := range complete {
			if name != key {
				environment[name] = value
			}
		}
		refuses(key+" unset", environment)
	}
	for _, key := range files {
		environment := map[string]string{}
		for name, value := range complete {
			environment[name] = value
		}
		environment[key] = empty
		refuses(key+" empty", environment)
	}
}

func memoryLimitAboveGoLimit(container object) bool {
	limit, err := resource.ParseQuantity(fmt.Sprint(at(container, "resources", "limits", "memory")))
	if err != nil {
		return false
	}
	for _, env := range container["env"].([]any) {
		if at(env, "name") == "GOMEMLIMIT" {
			soft, err := resource.ParseQuantity(strings.TrimSuffix(fmt.Sprint(at(env, "value")), "B"))
			return err == nil && soft.Cmp(limit) < 0
		}
	}
	return false
}

var cacheIdentity = regexp.MustCompile(`^ci-([a-z0-9][a-z0-9-]*)-(ro|rw|release)$`)

func objectStoreActions(identity string) []string {
	objectWriter := func(bucket string) []string {
		return []string{"Read:" + bucket, "Write:" + bucket + "/*", "List:" + bucket}
	}
	reader := func(bucket string) []string {
		return []string{"Read:" + bucket, "List:" + bucket}
	}
	switch identity {
	case "provisioner":
		return []string{"Admin"}
	case "toolchains-upload":
		return objectWriter("toolchains")
	}
	if match := cacheIdentity.FindStringSubmatch(identity); match != nil {
		switch match[2] {
		case "ro":
			return reader("ci-" + match[1] + "-main")
		case "rw":
			return objectWriter("ci-" + match[1] + "-main")
		case "release":
			return append(objectWriter("ci-"+match[1]+"-release"), reader("toolchains")...)
		}
	}
	return nil
}

func TestObjectStoreIdentitiesHoldExactlyTheirRoleActions(t *testing.T) {
	root := repoRoot(t)
	var spec struct {
		Cells []struct {
			Name    string
			Buckets []struct{ Name string }
		}
	}
	data, err := os.ReadFile(filepath.Join(root, objectStore, "buckets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range yamlObjects(t, data) {
		encoded, _ := json.Marshal(doc)
		if err := json.Unmarshal(encoded, &spec); err != nil {
			t.Fatal(err)
		}
	}
	buckets := map[string][]string{}
	for _, cell := range spec.Cells {
		for _, bucket := range cell.Buckets {
			buckets[cell.Name] = append(buckets[cell.Name], bucket.Name)
		}
	}
	reference := regexp.MustCompile(`^\$\{([A-Z0-9_]+)\}$`)
	resources := renderedTree(t, objectStore, objectStore)
	secrets := map[string]object{}
	for _, resource := range resources {
		if resource["kind"] == "Secret" {
			secrets[at(resource, "metadata", "name").(string)] = resource["stringData"].(object)
		}
	}
	for cell := range objectStoreCells {
		declared := map[string]bool{}
		for _, resource := range resources {
			if resource["kind"] != "StatefulSet" || at(resource, "metadata", "name") != "seaweedfs-"+cell {
				continue
			}
			for _, source := range at(resource, "spec", "template", "spec", "containers", 0, "envFrom").([]any) {
				reference, _ := source.(object)["secretRef"].(object)
				name, ok := reference["name"].(string)
				if !ok {
					continue
				}
				if secrets[name] == nil {
					t.Errorf("%s loads missing secret %s", cell, name)
				}
				for key := range secrets[name] {
					declared[key] = false
				}
			}
		}
		var config struct {
			Identities []struct {
				Name        string
				Credentials []struct{ AccessKey, SecretKey string }
				Actions     []string
			}
		}
		raw, err := os.ReadFile(filepath.Join(root, objectStore, cell+"-identities.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			t.Fatal(err)
		}
		for _, identity := range config.Identities {
			if len(identity.Credentials) != 1 {
				t.Errorf("%s/%s needs exactly one credential", cell, identity.Name)
			}
			for _, credential := range identity.Credentials {
				for _, value := range []string{credential.AccessKey, credential.SecretKey} {
					match := reference.FindStringSubmatch(value)
					if match == nil {
						t.Errorf("%s/%s stores a literal credential", cell, identity.Name)
						continue
					}
					if _, ok := declared[match[1]]; !ok {
						t.Errorf("%s/%s references undeclared %s", cell, identity.Name, match[1])
					}
					declared[match[1]] = true
					if owner := strings.ToUpper(strings.ReplaceAll(identity.Name, "-", "_")) + "_"; !strings.HasPrefix(match[1], owner) || strings.Count(strings.TrimPrefix(match[1], owner), "_") != 2 {
						t.Errorf("%s/%s authenticates with %s instead of its own %s credential", cell, identity.Name, match[1], owner)
					}
				}
			}
			want := objectStoreActions(identity.Name)
			if want == nil {
				t.Errorf("%s/%s has no declared role", cell, identity.Name)
				continue
			}
			got := slices.Sorted(slices.Values(identity.Actions))
			if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
				t.Errorf("%s/%s holds %v instead of %v", cell, identity.Name, got, want)
			}
			for _, action := range identity.Actions {
				if _, scope, scoped := strings.Cut(action, ":"); scoped && !slices.Contains(buckets[cell], strings.TrimSuffix(scope, "/*")) {
					t.Errorf("%s/%s grants %s outside the declared buckets", cell, identity.Name, action)
				}
			}
		}
		for key, used := range declared {
			if !used {
				t.Errorf("%s credential %s is unused", cell, key)
			}
		}
	}
	for identity, rejected := range map[string]string{"ci-nsql-ro": "Write:ci-nsql-main/*", "ci-nsql-rw": "Write:ci-nsql-release/*", "ci-nsql-release": "Write:toolchains/*"} {
		if slices.Contains(objectStoreActions(identity), rejected) {
			t.Errorf("%s role grants %s", identity, rejected)
		}
	}
}

func objectStoreCertificate(t *testing.T, file string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "pki", file))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s holds no certificate", file)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestObjectStoreCertificatesChainToTheCAAndMatchTheirAlert(t *testing.T) {
	authority := objectStoreCertificate(t, "ca.crt")
	roots := x509.NewCertPool()
	roots.AddCert(authority)
	earliest := authority.NotAfter
	for cell := range objectStoreCells {
		for kind, names := range map[string][]string{"s3": {"seaweedfs-" + cell + ".object-store.svc", "seaweedfs-" + cell + ".object-store.svc.cluster.local"}, "internal": {"localhost", "127.0.0.1"}} {
			certificate := objectStoreCertificate(t, cell+"-"+kind+".crt")
			for _, name := range names {
				if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
					t.Errorf("%s-%s does not serve %s: %v", cell, kind, name, err)
				}
			}
			if kind == "internal" && certificate.Subject.CommonName != "seaweedfs-"+cell+"-internal" {
				t.Errorf("%s internal certificate name %s", cell, certificate.Subject.CommonName)
			}
			if certificate.NotAfter.Before(earliest) {
				earliest = certificate.NotAfter
			}
		}
	}
	want := fmt.Sprintf("vector(time()) > %d", earliest.Add(-30*24*time.Hour).Unix())
	for _, resource := range objectStoreResources(t) {
		if resource["kind"] != "PrometheusRule" {
			continue
		}
		for _, rule := range at(resource, "spec", "groups", 0, "rules").([]any) {
			if at(rule, "alert") == "ObjectStoreCertificateExpiring" && strings.TrimSpace(fmt.Sprint(at(rule, "expr"))) != want {
				t.Errorf("certificate alert fires at %v instead of %s", at(rule, "expr"), want)
			}
		}
	}
}

func TestObjectStoreAuthorityIsConstrainedToTheObjectStore(t *testing.T) {
	authority := objectStoreCertificate(t, "ca.crt")
	var ranges []string
	for _, network := range authority.PermittedIPRanges {
		ranges = append(ranges, network.String())
	}
	if !authority.PermittedDNSDomainsCritical || !slices.Equal(authority.PermittedDNSDomains, []string{"object-store.svc", "object-store.svc.cluster.local", "localhost"}) || !slices.Equal(ranges, []string{"127.0.0.1/32"}) || len(authority.ExcludedDNSDomains)+len(authority.PermittedEmailAddresses)+len(authority.PermittedURIDomains) != 0 {
		t.Fatalf("CA name constraints: critical=%v dns=%v ip=%v", authority.PermittedDNSDomainsCritical, authority.PermittedDNSDomains, ranges)
	}
	if !authority.IsCA || authority.MaxPathLen != 0 || !authority.MaxPathLenZero {
		t.Fatal("object store CA may issue intermediates")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	constrained := *authority
	constrained.PublicKey = nil
	constrained.SerialNumber = big.NewInt(1)
	der, err := x509.CreateCertificate(rand.Reader, &constrained, &constrained, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(replica)
	issue := func(name string) *x509.Certificate {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, replica, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return leaf
	}
	if _, err := issue("seaweedfs-hel1.object-store.svc.cluster.local").Verify(x509.VerifyOptions{Roots: roots, DNSName: "seaweedfs-hel1.object-store.svc.cluster.local"}); err != nil {
		t.Fatalf("object store name rejected: %v", err)
	}
	for _, name := range []string{"github.com", "static.rust-lang.org", "object-store.svc.example.com"} {
		_, err := issue(name).Verify(x509.VerifyOptions{Roots: roots, DNSName: name})
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &invalid) || invalid.Reason != x509.CANotAuthorizedForThisName {
			t.Errorf("constrained CA vouches for %s: %v", name, err)
		}
	}
}

func canonicalPeer(t *testing.T, peer any) string {
	t.Helper()
	peer = clone(peer)
	if expressions, ok := lookup(peer, "podSelector", "matchExpressions").([]any); ok {
		for _, expression := range expressions {
			if values, ok := at(expression, "values").([]any); ok {
				slices.SortFunc(values, func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
			}
		}
	}
	encoded, err := json.Marshal(peer)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func selects(t *testing.T, selector any, podLabels map[string]string) bool {
	t.Helper()
	matchLabels, _ := at(selector, "matchLabels").(object)
	for key, value := range matchLabels {
		if podLabels[key] != value {
			return false
		}
	}
	expressions, _ := at(selector, "matchExpressions").([]any)
	for _, expression := range expressions {
		value, present := podLabels[at(expression, "key").(string)]
		values, _ := at(expression, "values").([]any)
		listed := slices.Contains(values, any(value))
		switch at(expression, "operator") {
		case "In":
			if !present || !listed {
				return false
			}
		case "NotIn":
			if present && listed {
				return false
			}
		case "Exists":
			if !present {
				return false
			}
		case "DoesNotExist":
			if present {
				return false
			}
		default:
			t.Fatalf("unknown selector operator %v", at(expression, "operator"))
		}
	}
	return true
}

var objectStorePeers = map[string]map[string][]object{
	"hel1": {
		"8333": {
			{"podSelector": object{"matchLabels": object{"app.kubernetes.io/name": "object-store-provisioner"}}},
			{"namespaceSelector": object{"matchLabels": object{"infra.fredrir.com/tier": "ci"}}, "podSelector": object{"matchExpressions": []any{object{"key": "actions.github.com/scale-set-name", "operator": "In", "values": []any{"rust-amd64", "rust-pr-amd64", "rust-release-amd64"}}}}},
		},
		"9327": {prometheusPeer},
		"9328": {prometheusPeer},
	},
}

var prometheusPeer = object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": "observability"}}, "podSelector": object{"matchLabels": object{"app.kubernetes.io/name": "prometheus"}}}

func TestObjectStoreAdmitsExactlyItsDeclaredPeers(t *testing.T) {
	resources := objectStoreResources(t)
	namespaced, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/network.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range yamlObjects(t, namespaced) {
		if resource["kind"] == "NetworkPolicy" && lookup(resource, "metadata", "namespace") == "object-store" {
			resources = append(resources, resource)
		}
	}
	podLabels := map[string]map[string]string{}
	for _, resource := range resources {
		if resource["kind"] == "StatefulSet" {
			values := map[string]string{}
			for key, value := range at(resource, "spec", "template", "metadata", "labels").(object) {
				values[key] = fmt.Sprint(value)
			}
			podLabels[strings.TrimPrefix(at(resource, "metadata", "name").(string), "seaweedfs-")] = values
		}
	}
	for cell := range objectStoreCells {
		want := map[string]map[string]bool{}
		for port, peers := range objectStorePeers[cell] {
			want[port] = map[string]bool{}
			for _, peer := range peers {
				want[port][canonicalPeer(t, peer)] = true
			}
		}
		got := map[string]map[string]bool{}
		for _, resource := range resources {
			if resource["kind"] != "NetworkPolicy" || !selects(t, at(resource, "spec", "podSelector"), podLabels[cell]) {
				continue
			}
			name := at(resource, "metadata", "name")
			if types, _ := at(resource, "spec", "policyTypes").([]any); !slices.Contains(types, any("Ingress")) && at(resource, "spec", "ingress") == nil {
				continue
			}
			rules, _ := at(resource, "spec", "ingress").([]any)
			for _, rule := range rules {
				ports, _ := at(rule, "ports").([]any)
				peers, _ := at(rule, "from").([]any)
				if len(ports) == 0 || len(peers) == 0 {
					t.Errorf("%s admits %s from any peer or on any port", name, cell)
					continue
				}
				for _, port := range ports {
					if at(port, "endPort") != nil || (at(port, "protocol") != nil && at(port, "protocol") != "TCP") {
						t.Errorf("%s opens a range or non-TCP port on %s", name, cell)
					}
					number := fmt.Sprint(at(port, "port"))
					if got[number] == nil {
						got[number] = map[string]bool{}
					}
					for _, peer := range peers {
						got[number][canonicalPeer(t, peer)] = true
					}
				}
			}
		}
		for port, peers := range got {
			for peer := range peers {
				if !want[port][peer] {
					t.Errorf("%s admits %s on %s", cell, peer, port)
				}
			}
		}
		for port, peers := range want {
			for peer := range peers {
				if !got[port][peer] {
					t.Errorf("%s no longer admits %s on %s", cell, peer, port)
				}
			}
		}
	}
}

func TestObjectStoreConfigMapsSkipFluxSubstitution(t *testing.T) {
	for _, resource := range objectStoreResources(t) {
		if resource["kind"] != "ConfigMap" {
			continue
		}
		data, _ := json.Marshal(resource["data"])
		if strings.Contains(string(data), "${") && lookup(resource, "metadata", "annotations", "kustomize.toolkit.fluxcd.io/substitute") != "disabled" {
			t.Errorf("Flux would substitute the references in %s", at(resource, "metadata", "name"))
		}
	}
}
