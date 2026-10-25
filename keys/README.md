# OpenSocks signing keys

This repository publishes its own apk feed for OpenWrt 25.12 and later. The apk
key below is the one that matters; the opkg/usign key is kept only so the IPK
output still verifies on 22.03–24.10 routers.

## apk signing key (OpenWrt 25.12+) — primary

`opensocks-apk.pem` is the public half of the repository's own EC P-256 key. It
is independent of any upstream or OpenWrt key: whoever owns this repository owns
this key. apk matches keys by identity rather than file name, so the file name is
only a convenience.

Clients trust the feed by installing the published copy next to their other keys:

```sh
wget -O /etc/apk/keys/opensocks.pem https://rel.n4t.su/opkg/apk/opensocks.pem
echo https://rel.n4t.su/opkg/apk/packages.adb >> /etc/apk/repositories.d/customfeeds.list
apk update
apk add opensocks-minimal luci-app-opensocks
```

### Creating your own key

Nothing ties this feed to the original author's key. A fork or a private build
can use its own pair; apk-tools 3 only needs the private key to sign the packages
and the matching public key to sign and verify the index:

```sh
openssl ecparam -name prime256v1 -genkey -noout -out private.pem
openssl ec -in private.pem -pubout -out public.pem
```

Then, for a local build:

```sh
APK_SIGNING_KEY_FILE=$PWD/private.pem \
APK_PUBLIC_KEY_FILE=$PWD/public.pem \
  ./tools/build-release.sh
```

For GitHub Actions, store the two PEM files as the repository secrets
`APK_SIGNING_KEY_PEM` and `APK_PUBLIC_KEY_PEM`. The workflow requires them: an
apk feed cannot be published unsigned (apk-tools 3 refuses to index a package
without a valid signature), so a run without the secret fails instead of shipping
an unusable feed. Replace `keys/opensocks-apk.pem` with your own public key so the
documented trust anchor matches what you publish.

The private key must never be committed. Only the public half belongs here.

### Rotation

There is no grace period: publishing with a new private key requires publishing
the matching public key at the same time, because clients reject an index whose
signature does not match a key already present in `/etc/apk/keys/`.

## opkg/usign key (OpenWrt 22.03–24.10) — optional

`d24a5e234001294c` is the upstream OpenSocks repository's public usign key, kept
for the IPK feed that older routers use:

```sh
wget -O /etc/opkg/keys/d24a5e234001294c https://rel.n4t.su/opkg/d24a5e234001294c
```

Its secret key is not in this repository. GitHub Actions loads it only from the
`OPKG_SIGNING_KEY` secret; without that secret the opkg index is published
unsigned (the workflow says so in its log), which only affects those older
routers. A fork does not own this key either: supply your own `OPKG_SIGNING_KEY`
secret, publish the matching public key as `keys/<fingerprint>`, and update the
`OPKG_PUBLIC_KEY_FILE` path in the workflow to point at it. Leaving the secret
unset is also fine — the IPK feed still builds and installs from local files,
it just cannot be verified as a repository by opkg.
