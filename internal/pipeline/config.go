package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

type Toolchain struct {
	Bazel       string `json:"bazel"`
	Go          string `json:"go"`
	Dagger      string `json:"dagger"`
	Image       string `json:"image"`
	BazelSHA256 string `json:"bazel_sha256"`
}

func ReadToolchain(root string) (Toolchain, error) {
	var result Toolchain
	data, err := os.ReadFile(filepath.Join(root, "build", "toolchain.json"))
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	version := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	if !version.MatchString(result.Bazel) || !version.MatchString(result.Go) || !version.MatchString(result.Dagger) {
		return result, errors.New("toolchain versions must be exact releases")
	}
	if !regexp.MustCompile(`@sha256:[a-f0-9]{64}$`).MatchString(result.Image) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(result.BazelSHA256) {
		return result, errors.New("toolchain image and Bazel download require SHA-256 pins")
	}
	pinned, err := os.ReadFile(filepath.Join(root, ".bazelversion"))
	if err != nil {
		return result, err
	}
	if string(pinned) != result.Bazel+"\n" {
		return result, fmt.Errorf(".bazelversion differs from toolchain Bazel %s", result.Bazel)
	}
	return result, nil
}
