package policy

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const stubRclone = `#!/bin/sh
printf '%s ' "$@" >> "$STUB_LOG"
echo >> "$STUB_LOG"
case "$1" in
  lsf)
    case "$*" in
      *"--format t"*) [ "$STUB_SENTINEL" = 1 ] && echo "$STUB_SEEDED_AT" ;;
      *) [ "$STUB_SENTINEL" = 1 ] && echo .mirror-seeded ;;
    esac
    ;;
  size)
    case "$2" in
      store:*) printf '{"count":%s,"bytes":1,"sizeless":0}\n' "$STUB_STORE_COUNT" ;;
      *) printf '{"count":%s,"bytes":1,"sizeless":0}\n' "$STUB_AWS_COUNT" ;;
    esac
    ;;
  check) exit "${STUB_CHECK_EXIT:-0}" ;;
esac
`

const datasetFilters = "--include=/files/** --include=/extract/** --include=/assets/** --include=/convert/** --fast-list"

func runMirrorScript(t *testing.T, mode string, environment map[string]string) ([]string, error) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "rclone"), []byte(stubRclone), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(directory, "calls")
	command := exec.Command("/bin/sh", filepath.Join(repoRoot(t), objectStore, "mirror.sh"), mode)
	command.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "STUB_LOG=" + log}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	data, _ := os.ReadFile(log)
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return calls, fmt.Errorf("%w: %s", err, output)
	}
	return calls, nil
}

func TestParserDatasetMirrorScriptGuardsTheAWSCopy(t *testing.T) {
	healthy := map[string]string{"MIN_SOURCE_PERCENT": "90", "STUB_SENTINEL": "1", "STUB_STORE_COUNT": "90", "STUB_AWS_COUNT": "100"}
	calls, err := runMirrorScript(t, "sync", healthy)
	if err != nil {
		t.Fatalf("seeded source refused: %v", err)
	}
	want := []string{
		"lsf --files-only --max-depth 1 --include=/.mirror-seeded store:parser-dataset",
		"size store:parser-dataset --json " + datasetFilters,
		"size aws:llunde-pyparser-bucket --json " + datasetFilters,
		"sync store:parser-dataset aws:llunde-pyparser-bucket --checksum --max-delete=1000 --log-level=NOTICE " + datasetFilters,
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("mirror ran\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	for name, change := range map[string][2]string{
		"unseeded source":     {"STUB_SENTINEL", "0"},
		"shrunken source":     {"STUB_STORE_COUNT", "89"},
		"empty source":        {"STUB_STORE_COUNT", "0"},
		"unreadable count":    {"STUB_STORE_COUNT", ""},
		"missing guard ratio": {"MIN_SOURCE_PERCENT", ""},
	} {
		environment := map[string]string{}
		for key, value := range healthy {
			environment[key] = value
		}
		environment[change[0]] = change[1]
		calls, err := runMirrorScript(t, "sync", environment)
		if err == nil || slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "sync ") }) {
			t.Errorf("%s: mirror synced (%v): %v", name, err, calls)
		}
	}
	if calls, err := runMirrorScript(t, "", healthy); err == nil || len(calls) != 0 {
		t.Errorf("mirror without a mode ran %v", calls)
	}
}

func TestParserDatasetMirrorDemandsASeedAfterTheParserLeftNL(t *testing.T) {
	fenced := map[string]string{"MIN_SOURCE_PERCENT": "90", "STUB_SENTINEL": "1", "STUB_STORE_COUNT": "100", "STUB_AWS_COUNT": "100", "SEED_NOT_BEFORE": "2026-09-27 12:00:00"}
	synced := func(calls []string) bool {
		return slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "sync ") })
	}
	for seeded, allowed := range map[string]bool{"2026-09-27 12:00:00": true, "2026-09-28 01:00:00": true, "2026-09-27 11:59:59": false, "2025-12-31 23:00:00": false, "": false} {
		environment := maps.Clone(fenced)
		environment["STUB_SEEDED_AT"] = seeded
		calls, err := runMirrorScript(t, "sync", environment)
		if allowed != (err == nil && synced(calls)) {
			t.Errorf("sentinel seeded at %q after the parser left at %s: synced=%v err=%v", seeded, fenced["SEED_NOT_BEFORE"], synced(calls), err)
		}
	}
	for _, fence := range []string{"0", "2026-09-27", "2026-09-27T12:00:00Z", "9999-99-99 99:99:99x"} {
		environment := maps.Clone(fenced)
		environment["STUB_SEEDED_AT"] = "2026-09-28 01:00:00"
		environment["SEED_NOT_BEFORE"] = fence
		if calls, err := runMirrorScript(t, "sync", environment); err == nil || synced(calls) {
			t.Errorf("malformed SEED_NOT_BEFORE %q accepted", fence)
		}
	}
}

func TestParserDatasetSeedMarksTheSourceOnlyAfterACheck(t *testing.T) {
	calls, err := runMirrorScript(t, "seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"copy aws:llunde-pyparser-bucket store:parser-dataset --checksum --transfers=16 --log-level=NOTICE " + datasetFilters,
		"check aws:llunde-pyparser-bucket store:parser-dataset --one-way --size-only " + datasetFilters,
		"delete --max-depth 1 --include=/.mirror-seeded store:parser-dataset",
		"touch store:parser-dataset/.mirror-seeded",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("seed ran\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	calls, err = runMirrorScript(t, "seed", map[string]string{"STUB_CHECK_EXIT": "1"})
	if err == nil || slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "touch ") }) {
		t.Fatalf("failed check still marked the source seeded: %v", calls)
	}
}

func TestParserDatasetRestoreKeepsEveryExistingObject(t *testing.T) {
	calls, err := runMirrorScript(t, "restore", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"copy aws:llunde-pyparser-bucket store:parser-dataset --ignore-existing --transfers=16 --log-level=NOTICE " + datasetFilters}
	if !slices.Equal(calls, want) {
		t.Fatalf("restore ran\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestParserDatasetUnseedRemovesOnlyTheSentinel(t *testing.T) {
	calls, err := runMirrorScript(t, "unseed", map[string]string{"STUB_SENTINEL": "0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"delete --max-depth 1 --include=/.mirror-seeded store:parser-dataset",
		"lsf --files-only --max-depth 1 --include=/.mirror-seeded store:parser-dataset",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("unseed ran\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if _, err := runMirrorScript(t, "unseed", map[string]string{"STUB_SENTINEL": "1"}); err == nil {
		t.Fatal("unseed reported success while the sentinel remained")
	}
}

func TestParserDatasetMirrorJobsKeepTheirBlastRadius(t *testing.T) {
	image := at(load(t, "platform/versions.yaml"), "images", "rclone")
	script, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "mirror.sh"))
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]struct {
		suspended  bool
		mode, cell string
		extra      map[string]string
	}{
		"parser-dataset-mirror": {false, "sync", "PARSER_MIRROR", map[string]string{"MIN_SOURCE_PERCENT": "90", "SEED_NOT_BEFORE": "configmap:parser-dataset-mirror-fence/SEED_NOT_BEFORE optional=true"}},
		"parser-dataset-seed":   {true, "seed", "PARSER_DATASET", nil},
	}
	resources := objectStoreResources(t)
	configMaps := map[string]object{}
	for _, resource := range resources {
		if resource["kind"] == "ConfigMap" {
			configMaps[at(resource, "metadata", "name").(string)] = resource
		}
	}
	found := map[string]bool{}
	for _, resource := range resources {
		name, _ := lookup(resource, "metadata", "name").(string)
		role, ok := roles[name]
		if resource["kind"] != "CronJob" || !ok {
			continue
		}
		found[name] = true
		if lookup(resource, "spec", "suspend") != role.suspended || lookup(resource, "spec", "concurrencyPolicy") != "Forbid" {
			t.Errorf("%s suspend=%v concurrency=%v", name, lookup(resource, "spec", "suspend"), lookup(resource, "spec", "concurrencyPolicy"))
		}
		pod := at(resource, "spec", "jobTemplate", "spec", "template", "spec").(object)
		if fmt.Sprint(pod["nodeSelector"]) != "map[kubernetes.io/arch:amd64 kubernetes.io/hostname:fredrir-04]" || pod["tolerations"] != nil || pod["affinity"] != nil || pod["nodeName"] != nil {
			t.Errorf("%s may leave fredrir-04: %v", name, pod["nodeSelector"])
		}
		if lookup(pod, "securityContext", "runAsNonRoot") != true || lookup(pod, "securityContext", "runAsUser") != 10001 || pod["automountServiceAccountToken"] != false || pod["hostNetwork"] != nil {
			t.Errorf("%s pod is not an unprivileged pod", name)
		}
		if at(resource, "spec", "jobTemplate", "spec", "template", "metadata", "labels", "app.kubernetes.io/name") != "parser-dataset-mirror" {
			t.Errorf("%s pods lose the network identity parser-dataset-mirror", name)
		}
		containers := pod["containers"].([]any)
		if len(containers) != 1 || pod["initContainers"] != nil {
			t.Fatalf("%s runs more than its rclone container", name)
		}
		container := containers[0].(object)
		if container["image"] != image {
			t.Errorf("%s runs %v instead of the pinned %v", name, container["image"], image)
		}
		if fmt.Sprint(container["command"]) != "[/bin/sh /etc/parser-dataset-mirror/mirror.sh]" || fmt.Sprint(container["args"]) != "["+role.mode+"]" {
			t.Errorf("%s runs %v %v", name, container["command"], container["args"])
		}
		if lookup(container, "securityContext", "allowPrivilegeEscalation") != false || lookup(container, "securityContext", "readOnlyRootFilesystem") != true || fmt.Sprint(lookup(container, "securityContext", "capabilities", "drop")) != "[ALL]" || lookup(container, "securityContext", "runAsUser") != nil {
			t.Errorf("%s container is not hardened", name)
		}
		environment := map[string]string{}
		for _, env := range container["env"].([]any) {
			key := at(env, "name").(string)
			if reference, ok := lookup(env, "valueFrom", "secretKeyRef").(object); ok {
				environment[key] = fmt.Sprint(reference["name"]) + "/" + fmt.Sprint(reference["key"])
			} else if reference, ok := lookup(env, "valueFrom", "configMapKeyRef").(object); ok {
				environment[key] = fmt.Sprint("configmap:", reference["name"], "/", reference["key"], " optional=", reference["optional"])
			} else {
				environment[key] = fmt.Sprint(at(env, "value"))
			}
		}
		want := map[string]string{
			"HOME":                                  "/tmp",
			"GOMEMLIMIT":                            "192MiB",
			"SSL_CERT_DIR":                          "/etc/ssl/certs:/etc/object-store",
			"RCLONE_CONFIG_STORE_TYPE":              "s3",
			"RCLONE_CONFIG_STORE_PROVIDER":          "SeaweedFS",
			"RCLONE_CONFIG_STORE_ENDPOINT":          "https://seaweedfs-nl.object-store.svc.cluster.local:8333",
			"RCLONE_CONFIG_STORE_REGION":            "nl",
			"RCLONE_CONFIG_STORE_NO_CHECK_BUCKET":   "true",
			"RCLONE_CONFIG_STORE_ACCESS_KEY_ID":     "seaweedfs-nl-identities/" + role.cell + "_ACCESS_KEY_ID",
			"RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY": "seaweedfs-nl-identities/" + role.cell + "_SECRET_ACCESS_KEY",
			"RCLONE_CONFIG_AWS_TYPE":                "s3",
			"RCLONE_CONFIG_AWS_PROVIDER":            "AWS",
			"RCLONE_CONFIG_AWS_REGION":              "eu-north-1",
			"RCLONE_CONFIG_AWS_NO_CHECK_BUCKET":     "true",
			"RCLONE_CONFIG_AWS_ACCESS_KEY_ID":       "parser-dataset-aws/AWS_ACCESS_KEY_ID",
			"RCLONE_CONFIG_AWS_SECRET_ACCESS_KEY":   "parser-dataset-aws/AWS_SECRET_ACCESS_KEY",
		}
		for key, value := range role.extra {
			want[key] = value
		}
		if fmt.Sprint(environment) != fmt.Sprint(want) || container["envFrom"] != nil {
			t.Errorf("%s environment\n%v\nwant\n%v", name, environment, want)
		}
		mounted := false
		for _, volume := range pod["volumes"].([]any) {
			reference, _ := lookup(volume, "configMap", "name").(string)
			if at(volume, "name") == "script" {
				mounted = configMaps[reference] != nil && lookup(configMaps[reference], "data", "mirror.sh") == string(script)
			}
		}
		if !mounted {
			t.Errorf("%s does not run the reviewed mirror.sh", name)
		}
	}
	if len(found) != len(roles) {
		t.Fatalf("mirror jobs: %v", found)
	}
	var egress []string
	for _, resource := range resources {
		if resource["kind"] != "NetworkPolicy" || !selects(t, at(resource, "spec", "podSelector"), map[string]string{"app.kubernetes.io/name": "parser-dataset-mirror"}) {
			continue
		}
		rules, _ := lookup(resource, "spec", "egress").([]any)
		for _, rule := range rules {
			for _, peer := range at(rule, "to").([]any) {
				for _, port := range at(rule, "ports").([]any) {
					egress = append(egress, canonicalPeer(t, peer)+" "+fmt.Sprint(at(port, "port"))+"/"+fmt.Sprint(at(port, "protocol")))
				}
			}
		}
	}
	slices.Sort(egress)
	wantEgress := []string{
		canonicalPeer(t, object{"ipBlock": object{"cidr": "0.0.0.0/0", "except": []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8"}}}) + " 443/TCP",
		canonicalPeer(t, object{"podSelector": object{"matchLabels": object{"app.kubernetes.io/name": "seaweedfs", "app.kubernetes.io/instance": "nl"}}}) + " 8333/TCP",
	}
	slices.Sort(wantEgress)
	if !slices.Equal(egress, wantEgress) {
		t.Errorf("mirror egress\n%s\nwant\n%s", strings.Join(egress, "\n"), strings.Join(wantEgress, "\n"))
	}
}
