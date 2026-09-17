#!/usr/bin/env bash
set -euo pipefail
cd "$1"
GNUPGHOME=$(mktemp -d)
export GNUPGHOME
gpg --batch --quiet --import /run/secrets/gpg
key=$(gpg --batch --with-colons --list-secret-keys | awk -F: '/^fpr:/ {print $10; exit}')
for arch in amd64 arm64; do
  dir="dists/stable/main/binary-$arch"
  mkdir -p "$dir/by-hash/SHA256"
  apt-ftparchive --arch "$arch" packages pool > "$dir/Packages"
  gzip -9nk "$dir/Packages"
  for file in Packages Packages.gz; do
    cp "$dir/$file" "$dir/by-hash/SHA256/$(sha256sum "$dir/$file" | cut -d' ' -f1)"
  done
done
apt-ftparchive \
  -o APT::FTPArchive::Release::Origin=fredrir \
  -o APT::FTPArchive::Release::Label=fredrir \
  -o APT::FTPArchive::Release::Suite=stable \
  -o APT::FTPArchive::Release::Codename=stable \
  -o "APT::FTPArchive::Release::Architectures=amd64 arm64" \
  -o APT::FTPArchive::Release::Components=main \
  -o "APT::FTPArchive::Release::Description=fredrir packages" \
  -o APT::FTPArchive::Release::Acquire-By-Hash=yes \
  release dists/stable > "$GNUPGHOME/Release"
mv "$GNUPGHOME/Release" dists/stable/Release
gpg --batch --yes --local-user "$key" --digest-algo SHA512 --clearsign --output dists/stable/InRelease dists/stable/Release
gpg --batch --yes --local-user "$key" --digest-algo SHA512 --armor --detach-sign --output dists/stable/Release.gpg dists/stable/Release
