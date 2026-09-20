package ci

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"slices"
)

func NixHash(args []string, output io.Writer) error {
	if len(args) != 5 || !slices.Equal(args[:4], []string{"--type", "sha256", "--flat", "--base32"}) {
		return fmt.Errorf("usage: nix-hash --type sha256 --flat --base32 FILE")
	}
	file, err := os.Open(args[4])
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	digest := hash.Sum(nil)
	const alphabet = "0123456789abcdfghijklmnpqrsvwxyz"
	encoded := make([]byte, 0, 52)
	for index := 51; index >= 0; index-- {
		byteIndex, offset := index*5/8, uint(index*5%8)
		value := digest[byteIndex] >> offset
		if byteIndex+1 < len(digest) {
			value |= digest[byteIndex+1] << (8 - offset)
		}
		encoded = append(encoded, alphabet[value&31])
	}
	_, err = fmt.Fprintln(output, string(encoded))
	return err
}
