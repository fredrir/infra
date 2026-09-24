package packages

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

func (commands *verificationCommands) Run(_ context.Context, name string, args ...string) error {
	commands.calls = append(commands.calls, append([]string{name}, args...))
	return nil
}
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
	assets    map[string][]byte
	verified  map[string]bool
	tags      []string
	tamperTag string
}

func (source *releaseFixture) Releases(context.Context, string) ([]PublishedRelease, error) {
	if len(source.tags) > 0 {
		var releases []PublishedRelease
		for _, tag := range source.tags {
			releases = append(releases, published(tag, false, false, "release.json"))
		}
		return releases, nil
	}
	return []PublishedRelease{published("v1.2.3", false, false, "release.json")}, nil
}
func (source *releaseFixture) Download(_ context.Context, _, tag, destination string, channels bool) error {
	source.verified = map[string]bool{}
	for name, data := range source.assets {
		if !channels && slices.ContainsFunc(channelSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) }) {
			continue
		}
		if tag == source.tamperTag && strings.HasSuffix(name, ".tar.gz") {
			data = []byte("tampered")
		}
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
	source   *releaseFixture
	built    int
	versions []string
}

func (builder *packageFixture) Build(_ context.Context, settings release.Settings, version, directory, _, _ string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "checksums.txt" && !builder.source.verified[entry.Name()] {
			return fmt.Errorf("packaging ran before %s was verified", entry.Name())
		}
	}
	if settings.Name != "tool" {
		return fmt.Errorf("incorrect package identity")
	}
	builder.built++
	builder.versions = append(builder.versions, version)
	return nil
}

func TestCollectionVerifiesEveryAssetBeforePackaging(t *testing.T) {
	for _, scenario := range []string{"valid", "tampered", "mismatched", "history", "history-tampered"} {
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
			if strings.HasPrefix(scenario, "history") {
				source.tags = []string{"v1.2.1", "v1.2.3", "v1.2.2"}
			}
			if scenario == "history-tampered" {
				source.tamperTag = "v1.2.2"
			}
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
			if scenario == "history-tampered" {
				if err == nil || !slices.Equal(builder.versions, []string{"1.2.3"}) {
					t.Fatal("packaged corrupt historical assets")
				}
				return
			}
			if scenario == "tampered" || scenario == "mismatched" {
				if err == nil || builder.built != 0 {
					t.Fatal("packaged corrupt assets")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantVersions := []string{"1.2.3"}
			if scenario == "history" {
				wantVersions = append(wantVersions, "1.2.2", "1.2.1")
				for _, tag := range []string{"v1.2.2", "v1.2.1"} {
					for _, suffix := range channelSuffixes {
						if _, err := os.Stat(filepath.Join(root, "work/releases/tool", tag, "tool"+suffix)); !os.IsNotExist(err) {
							t.Fatalf("unused historical channel downloaded: %s%s", tag, suffix)
						}
					}
				}
			}
			if !slices.Equal(builder.versions, wantVersions) || tools["tool"].Version != "1.2.3" || !reflect.DeepEqual(tools["tool"].Taps, []string{"homebrew-tool"}) {
				t.Fatalf("incorrect collection %v", tools)
			}
			if _, err := os.Stat(filepath.Join(root, "channels/tool/tool-bin.pkgbuild")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDownloadRetainsPackageVerificationInputsWithoutHistoricalChannels(t *testing.T) {
	for _, channels := range []bool{false, true} {
		commands := &verificationCommands{}
		source := &GitHub{Runner: commands}
		if err := source.Download(context.Background(), "fredrir/tool", "v1.2.3", t.TempDir(), channels); err != nil {
			t.Fatal(err)
		}
		args := commands.calls[0]
		for _, pattern := range []string{"*-unknown-linux-*.tar.gz", "checksums.txt", "release.json"} {
			if !slices.Contains(args, pattern) {
				t.Fatalf("missing package input %s", pattern)
			}
		}
		for _, pattern := range []string{"*.rb", "*.nix", "*.pkgbuild", "*.srcinfo"} {
			if slices.Contains(args, pattern) != channels {
				t.Fatalf("incorrect channel selection for %s", pattern)
			}
		}
	}
}
