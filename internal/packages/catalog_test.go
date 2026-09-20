package packages

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/release"
)

func published(tag string, draft, prerelease bool, assets ...string) PublishedRelease {
	value := PublishedRelease{Tag: tag, Draft: draft, Prerelease: prerelease}
	for _, name := range assets {
		value.Assets = append(value.Assets, struct{ Name string }{Name: name})
	}
	return value
}

func TestStableReleasesSelectNewestPackageBearingVersions(t *testing.T) {
	releases := []PublishedRelease{published("v0.1.9", false, false, "release.json"), published("v0.1.10", false, false, "release.json"), published("v0.2.0-rc.1", false, true, "release.json"), published("v0.1.11", true, false, "release.json"), published("v0.1.8", false, false, "release.json"), published("v0.1.13", false, false, "tool.tar.xz"), published("nightly", false, false, "release.json")}
	if got := StableReleases(releases, 3); !reflect.DeepEqual(got, []string{"v0.1.10", "v0.1.9", "v0.1.8"}) {
		t.Fatalf("selected %v", got)
	}
}

func TestChecksumsAcceptBinaryMarkersAndRejectDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checksums.txt")
	input := strings.Repeat("1", 64) + "  a.tar.gz\n" + strings.Repeat("2", 64) + " *b.rb\n"
	if err := os.WriteFile(path, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}
	entries, err := Checksums(path)
	if err != nil || entries["a.tar.gz"] != strings.Repeat("1", 64) || entries["b.rb"] != strings.Repeat("2", 64) {
		t.Fatalf("%v %v", entries, err)
	}
	if err := os.WriteFile(path, []byte(input+strings.Repeat("3", 64)+"  a.tar.gz\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Checksums(path); err == nil {
		t.Fatal("accepted duplicate checksum")
	}
}

func TestAttestationRequiresValidSignerDigests(t *testing.T) {
	data := []byte(`[{"verificationResult":{"signature":{"certificate":{"buildSignerDigest":"` + strings.Repeat("a", 40) + `"}}}}]`)
	digests, err := SignerDigests(data)
	if err != nil || len(digests) != 1 || digests[0] != strings.Repeat("a", 40) {
		t.Fatalf("%v %v", digests, err)
	}
	for _, data := range []string{`[]`, `[{}]`, `[{"verificationResult":{"signature":{"certificate":{"buildSignerDigest":"main"}}}}]`} {
		if _, err := SignerDigests([]byte(data)); err == nil {
			t.Fatal("accepted unverified signer")
		}
	}
}

type verificationCommands struct {
	status string
	calls  [][]string
}

func (commands *verificationCommands) Run(context.Context, string, ...string) error { return nil }
func (commands *verificationCommands) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	commands.calls = append(commands.calls, append([]string{name}, args...))
	switch args[0] {
	case "attestation":
		return []byte(`[{"verificationResult":{"signature":{"certificate":{"buildSignerDigest":"` + strings.Repeat("a", 40) + `"}}}}]`), nil
	case "api":
		return json.Marshal(map[string]string{"status": commands.status})
	default:
		return nil, fmt.Errorf("unexpected command")
	}
}

func TestAssetVerificationRequiresSignerRevisionOnMain(t *testing.T) {
	for _, status := range []string{"ahead", "identical", "behind", "diverged"} {
		t.Run(status, func(t *testing.T) {
			commands := &verificationCommands{status: status}
			source := &GitHub{Runner: commands}
			err := source.Verify(context.Background(), "asset.tar.gz", "fredrir/tool", "v1.2.3")
			accepted := status == "ahead" || status == "identical"
			if (err == nil) != accepted {
				t.Fatalf("status %s: %v", status, err)
			}
			args := strings.Join(commands.calls[0], " ")
			if !strings.Contains(args, "--signer-workflow fredrir/infra/.github/workflows/rust-release.yml") || !strings.Contains(args, "--source-ref refs/tags/v1.2.3") {
				t.Fatalf("verification lost provenance constraints: %s", args)
			}
		})
	}
}

type releaseFixture struct {
	assets   map[string][]byte
	verified map[string]bool
}

func (source *releaseFixture) Releases(context.Context, string) ([]PublishedRelease, error) {
	return []PublishedRelease{published("v1.2.3", false, false, "release.json")}, nil
}
func (source *releaseFixture) Download(_ context.Context, _, _, destination string) error {
	for name, data := range source.assets {
		if err := os.WriteFile(filepath.Join(destination, name), data, 0644); err != nil {
			return err
		}
	}
	return nil
}
func (source *releaseFixture) Verify(_ context.Context, path, _, _ string) error {
	source.verified[filepath.Base(path)] = true
	return nil
}

type packageFixture struct {
	source *releaseFixture
	built  int
}

func (builder *packageFixture) Build(_ context.Context, settings release.Settings, version, _, _, _ string) error {
	if len(builder.source.verified) != len(builder.source.assets)-1 {
		return fmt.Errorf("packaging ran before every asset was verified")
	}
	if settings.Name != "tool" || version != "1.2.3" {
		return fmt.Errorf("incorrect package identity")
	}
	builder.built++
	return nil
}

func TestCollectionVerifiesEveryAssetBeforePackaging(t *testing.T) {
	for _, scenario := range []string{"valid", "tampered", "mismatched"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			registry := filepath.Join(root, "registry.yaml")
			if err := os.WriteFile(registry, []byte("projects:\n- project: tool\n  repository: fredrir/tool\n  visibility: public\n  taps: [homebrew-tool]\n- project: secret\n  repository: fredrir/secret\n  visibility: private\n"), 0644); err != nil {
				t.Fatal(err)
			}
			repository := "fredrir/tool"
			if scenario == "mismatched" {
				repository = "fredrir/other"
			}
			summary, _ := json.Marshal(release.Settings{Name: "tool", Binary: "tool", Repository: repository})
			source := &releaseFixture{assets: map[string][]byte{"release.json": summary, "tool-x86_64-unknown-linux-gnu-v1.2.3.tar.gz": []byte("archive")}, verified: map[string]bool{}}
			for _, suffix := range channelSuffixes {
				source.assets["tool"+suffix] = []byte("channel")
			}
			var checksums strings.Builder
			for name, data := range source.assets {
				fmt.Fprintf(&checksums, "%x  %s\n", sha256.Sum256(data), name)
			}
			source.assets["checksums.txt"] = []byte(checksums.String())
			if scenario == "tampered" {
				source.assets["release.json"] = []byte("tampered")
			}
			builder := &packageFixture{source: source}
			tools, err := Collect(context.Background(), source, builder, CollectOptions{Registry: registry, Work: filepath.Join(root, "work"), Site: filepath.Join(root, "site"), Channels: filepath.Join(root, "channels")})
			if scenario != "valid" {
				if err == nil || builder.built != 0 {
					t.Fatal("packaged corrupt assets")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if builder.built != 1 || tools["tool"].Version != "1.2.3" || !reflect.DeepEqual(tools["tool"].Taps, []string{"homebrew-tool"}) {
				t.Fatalf("incorrect collection %v", tools)
			}
			if _, err := os.Stat(filepath.Join(root, "channels/tool/tool-bin.pkgbuild")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
